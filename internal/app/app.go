// Package app is the daemon core: it wires the adb client, profile databases,
// notifier, tray and viewer together. It monitors device connect/disconnect
// events and, while a phone that belongs to a profile is connected,
// incrementally imports new SMS into that profile's database with no busy
// waiting and clean per-device cancellation. A phone in no profile is not
// touched until the user assigns it one from the tray.
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jbinder/smsd/internal/adb"
	"github.com/jbinder/smsd/internal/config"
	"github.com/jbinder/smsd/internal/database"
	"github.com/jbinder/smsd/internal/logging"
	"github.com/jbinder/smsd/internal/notify"
	"github.com/jbinder/smsd/internal/profile"
	"github.com/jbinder/smsd/internal/sms"
	"github.com/jbinder/smsd/internal/tray"
	"github.com/jbinder/smsd/internal/ui"
)

// App holds the daemon's long-lived collaborators and per-device state.
type App struct {
	cfg      config.Config
	log      *logging.Logger
	adb      *adb.Client
	profiles *profile.Store
	dbDir    string // where the profile databases live
	note     *notify.Notifier
	ui       *ui.Server
	tray     *tray.Tray

	mu       sync.Mutex
	ctx      context.Context         // Run's context, for loops started from the tray
	dbs      map[string]*database.DB // open databases by profile id
	ids      map[string]string       // phone serial number by adb connection, learned once per connection
	devices  map[string]*deviceSync  // active sync loops by phone serial number
	pending  map[string]phone        // connected phones in no profile, awaiting a choice
	ignoring map[string]bool         // connected phones set to be ignored, logged once
	adbError bool                    // last device-list poll failed
	wg       sync.WaitGroup
}

// phone is a connected phone: its serial number, which identifies it in the
// profiles and databases, and the adb connection it is reached through. Over
// USB the two are the same; over Wi-Fi the connection is an address.
type phone struct {
	id   string
	conn adb.Device
}

// deviceSync tracks one connected device's sync goroutine.
type deviceSync struct {
	serial  string       // the phone's serial number, its name in the database
	conn    string       // the adb connection, for running commands
	user    int          // Android user whose contacts and call log are read
	profile string       // id of the profile the device belongs to
	db      *database.DB // that profile's database
	cancel  context.CancelFunc
	poke    chan struct{} // buffered: a manual Refresh triggers an immediate poll
}

// New constructs an App and opens every profile's database. The tray is
// attached separately via SetTray because its lifecycle is owned by main (it
// must run on the main goroutine).
func New(cfg config.Config, log *logging.Logger, client *adb.Client, profiles *profile.Store, dbDir string, note *notify.Notifier, viewer *ui.Server) (*App, error) {
	a := &App{
		cfg:      cfg,
		log:      log,
		adb:      client,
		profiles: profiles,
		dbDir:    dbDir,
		note:     note,
		ui:       viewer,
		ctx:      context.Background(),
		dbs:      make(map[string]*database.DB),
		ids:      make(map[string]string),
		devices:  make(map[string]*deviceSync),
		pending:  make(map[string]phone),
		ignoring: make(map[string]bool),
	}
	for _, p := range profiles.List() {
		if _, err := a.openDB(p); err != nil {
			a.Close()
			return nil, err
		}
	}
	return a, nil
}

// Close closes the profile databases. Call it after Run has returned.
func (a *App) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, db := range a.dbs {
		db.Close()
		delete(a.dbs, id)
	}
}

// openDB opens p's database, creating it if needed.
func (a *App) openDB(p profile.Profile) (*database.DB, error) {
	a.mu.Lock()
	db := a.dbs[p.ID]
	a.mu.Unlock()
	if db != nil {
		return db, nil
	}
	db, err := database.Open(profile.DBPath(a.dbDir, p))
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	a.mu.Lock()
	a.dbs[p.ID] = db
	a.mu.Unlock()
	return db, nil
}

// allDBs returns every open profile database.
func (a *App) allDBs() []*database.DB {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*database.DB, 0, len(a.dbs))
	for _, db := range a.dbs {
		out = append(out, db)
	}
	return out
}

// SetTray attaches the tray so the App can reflect state changes on it.
func (a *App) SetTray(t *tray.Tray) { a.tray = t }

// Run starts the adb server and the device-monitor loop, blocking until ctx is
// cancelled. It returns after all per-device goroutines have stopped.
func (a *App) Run(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()

	if err := a.adb.StartServer(ctx); err != nil {
		a.log.Errorf("starting adb server: %v", err)
		a.setState(tray.StateError, "adb server failed to start")
	} else {
		a.log.Infof("adb server started")
	}

	ticker := time.NewTicker(time.Duration(a.cfg.DevicePollSeconds) * time.Second)
	defer ticker.Stop()

	a.scanDevices(ctx) // immediate first scan
	for {
		select {
		case <-ctx.Done():
			a.stopAllDevices()
			a.wg.Wait()
			a.log.Infof("device monitor stopped")
			return
		case <-ticker.C:
			a.scanDevices(ctx)
		}
	}
}

// scanDevices reconciles the set of connected devices with running sync loops.
// It only performs the cheap local `adb devices` call, plus one getprop the
// first time a connection appears; SMS polling happens in per-device
// goroutines and only while connected.
func (a *App) scanDevices(ctx context.Context) {
	devs, err := a.adb.Devices(ctx)
	if err != nil {
		a.log.Warnf("listing devices: %v", err)
		a.mu.Lock()
		a.adbError = true
		a.mu.Unlock()
		a.refreshState()
		return
	}

	// A phone plugged in over USB and connected over Wi-Fi is listed twice;
	// it is one phone, known by its serial number.
	conns := map[string][]adb.Device{} // by phone serial number
	unusable := false
	listed := map[string]bool{}
	for _, d := range devs {
		listed[d.Serial] = true
		if !d.Connected() {
			// e.g. "unauthorized" or "offline": surfaced as an error state.
			unusable = true
			a.log.Warnf("device %s is %s", d.Serial, d.State)
			continue
		}
		id, ok := a.phoneID(ctx, d.Serial)
		if !ok {
			unusable = true
			continue
		}
		conns[id] = append(conns[id], d)
	}

	var arrived []phone
	pendingChanged := false
	a.mu.Lock()
	a.adbError = unusable
	for conn := range a.ids {
		if !listed[conn] {
			delete(a.ids, conn)
		}
	}
	// Cancel loops whose connection went away. A phone still reachable over
	// another connection is picked up again below.
	for id, ds := range a.devices {
		if !slices.ContainsFunc(conns[id], func(d adb.Device) bool { return d.Serial == ds.conn }) {
			if len(conns[id]) == 0 {
				a.log.Infof("device %s disconnected", id)
			} else {
				a.log.Infof("device %s: connection %s went away", id, ds.conn)
			}
			ds.cancel()
			delete(a.devices, id)
		}
	}
	// Start loops for newly connected devices that belong to a profile; hold
	// back the rest.
	for id, cs := range conns {
		if _, ok := a.devices[id]; ok {
			continue
		}
		ph := phone{id: id, conn: cs[0]}
		if p, ok := a.profiles.ForDevice(id); ok {
			a.startDevice(ctx, ph, p)
			continue
		}
		if a.profiles.Ignored(id) {
			if !a.ignoring[id] {
				a.log.Infof("device %s connected, ignored (see profiles.json)", id)
				a.ignoring[id] = true
			}
			continue
		}
		if _, ok := a.pending[id]; !ok {
			a.log.Infof("device %s connected, belongs to no profile: not syncing until one is chosen", phoneLabel(ph))
			a.pending[id] = ph
			arrived = append(arrived, ph)
			pendingChanged = true
		}
	}
	for id, ph := range a.pending {
		if len(conns[id]) == 0 {
			a.log.Infof("device %s disconnected without a profile", id)
			delete(a.pending, id)
			pendingChanged = true
		} else if !slices.Contains(conns[id], ph.conn) {
			a.pending[id] = phone{id: id, conn: conns[id][0]}
		}
	}
	for id := range a.ignoring {
		if len(conns[id]) == 0 {
			delete(a.ignoring, id)
		}
	}
	a.mu.Unlock()

	for _, ph := range arrived {
		a.note.Notify("New phone connected",
			phoneLabel(ph)+" is not synced. Choose a profile for it in the smsd tray menu.")
	}
	if pendingChanged {
		a.UpdateTrayMenu()
	}
	a.refreshState()
}

// phoneID returns the serial number of the phone on adb connection conn,
// asking the phone the first time the connection is seen. It reports false
// when the phone could not be asked; the next scan tries again.
func (a *App) phoneID(ctx context.Context, conn string) (string, bool) {
	a.mu.Lock()
	id, ok := a.ids[conn]
	a.mu.Unlock()
	if ok {
		return id, true
	}
	id, err := a.adb.SerialNo(ctx, conn)
	if err != nil {
		a.log.Warnf("device %s: reading serial number: %v", conn, err)
		return "", false
	}
	if id == "" {
		// Some builds leave it unset; the connection name is all there is.
		id = conn
	}
	if id != conn {
		a.log.Infof("device %s is phone %s", conn, id)
	}
	a.mu.Lock()
	a.ids[conn] = id
	a.mu.Unlock()
	return id, true
}

// startDevice launches a sync goroutine for ph into p's database. Caller
// must hold a.mu.
func (a *App) startDevice(parent context.Context, ph phone, p profile.Profile) {
	db := a.dbs[p.ID]
	if db == nil {
		a.log.Errorf("device %s: database of profile %s is not open", ph.id, p.Name)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	ds := &deviceSync{serial: ph.id, conn: ph.conn.Serial, user: p.User(ph.id), profile: p.ID,
		db: db, cancel: cancel, poke: make(chan struct{}, 1)}
	a.devices[ph.id] = ds
	via := ""
	if ds.conn != ds.serial {
		via = " via " + ds.conn
	}
	a.log.Infof("device %s connected%s, starting sync of Android user %d into profile %s",
		ds.serial, via, ds.user, p.Name)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.syncDevice(ctx, ds)
	}()
}

// phoneLabel names a phone for the user, e.g. "XQ_BT52 (HQ617A1F6F)".
func phoneLabel(ph phone) string {
	if ph.conn.Model == "" {
		return ph.id
	}
	return ph.conn.Model + " (" + ph.id + ")"
}

// --- profiles ---------------------------------------------------------------

// UpdateTrayMenu lists the phones waiting for a profile in the tray menu.
func (a *App) UpdateTrayMenu() {
	if a.tray == nil {
		return
	}
	a.mu.Lock()
	phones := make([]tray.Pending, 0, len(a.pending))
	for _, ph := range a.pending {
		phones = append(phones, tray.Pending{Serial: ph.id, Label: phoneLabel(ph)})
	}
	a.mu.Unlock()
	sort.Slice(phones, func(i, j int) bool { return phones[i].Serial < phones[j].Serial })

	var choices []tray.Choice
	for _, p := range a.profiles.List() {
		choices = append(choices, tray.Choice{ID: p.ID, Name: p.Name})
	}
	a.tray.SetPending(phones, choices)
}

// takePending removes serial from the phones awaiting a profile.
func (a *App) takePending(serial string) (phone, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ph, ok := a.pending[serial]
	delete(a.pending, serial)
	return ph, ok
}

// foregroundUser returns the Android user in the foreground on ph. That is
// the person handling the phone while it is being set up, so it is whose
// contacts and call log the profile gets.
func (a *App) foregroundUser(ph phone) (int, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	user, err := a.adb.CurrentUser(ctx, ph.conn.Serial)
	if err != nil {
		return 0, fmt.Errorf("reading the phone's current user: %w", err)
	}
	return user, nil
}

// NewProfile puts a waiting phone into a new profile of its own, named after
// its model, and starts syncing it.
func (a *App) NewProfile(serial string) {
	ph, ok := a.takePending(serial)
	if !ok {
		return
	}
	name := ph.conn.Model
	if name == "" {
		name = serial
	}
	var p profile.Profile
	user, err := a.foregroundUser(ph)
	if err == nil {
		p, err = a.profiles.Create(name, user, serial)
	}
	if err == nil {
		_, err = a.openDB(p)
	}
	if err != nil {
		a.log.Errorf("device %s: creating profile: %v", serial, err)
		a.note.Notify("smsd", "Could not create a profile for "+phoneLabel(ph)+": "+err.Error())
		a.restorePending(ph)
		return
	}
	a.log.Infof("device %s: created profile %s (%s) for Android user %d", serial, p.Name, p.ID, user)
	a.adopt(ph, p)
}

// Attach adds a waiting phone to an existing profile, e.g. the replacement
// for a phone already in it, and starts syncing it.
func (a *App) Attach(serial, profileID string) {
	ph, ok := a.takePending(serial)
	if !ok {
		return
	}
	var p profile.Profile
	user, err := a.foregroundUser(ph)
	if err == nil {
		p, err = a.profiles.Attach(profileID, serial, user)
	}
	if err != nil {
		a.log.Errorf("device %s: adding to profile %s: %v", serial, profileID, err)
		a.note.Notify("smsd", "Could not add "+phoneLabel(ph)+" to the profile: "+err.Error())
		a.restorePending(ph)
		return
	}
	a.log.Infof("device %s: added to profile %s for Android user %d", serial, p.Name, user)
	a.adopt(ph, p)
}

// Ignore stops offering a waiting phone; it is never synced.
func (a *App) Ignore(serial string) {
	ph, ok := a.takePending(serial)
	if !ok {
		return
	}
	if err := a.profiles.Ignore(serial); err != nil {
		a.log.Errorf("device %s: ignoring: %v", serial, err)
		a.restorePending(ph)
		return
	}
	a.mu.Lock()
	a.ignoring[serial] = true
	a.mu.Unlock()
	a.log.Infof("device %s: ignored from now on", serial)
	a.UpdateTrayMenu()
	a.refreshState()
}

// restorePending puts ph back among the waiting phones after a failed choice.
func (a *App) restorePending(ph phone) {
	a.mu.Lock()
	a.pending[ph.id] = ph
	a.mu.Unlock()
	a.UpdateTrayMenu()
}

// adopt starts syncing ph into p now that it has a profile.
func (a *App) adopt(ph phone, p profile.Profile) {
	a.mu.Lock()
	if _, ok := a.devices[ph.id]; !ok {
		a.startDevice(a.ctx, ph, p)
	}
	a.mu.Unlock()
	a.UpdateTrayMenu()
	a.refreshState()
}

// Profiles describes every profile for the viewer's profile switcher.
func (a *App) Profiles() []ui.ProfileInfo {
	list := a.profiles.List()
	a.mu.Lock()
	connected := map[string]bool{}
	for _, ds := range a.devices {
		connected[ds.profile] = true
	}
	dbs := make(map[string]*database.DB, len(a.dbs))
	for id, db := range a.dbs {
		dbs[id] = db
	}
	a.mu.Unlock()

	out := make([]ui.ProfileInfo, 0, len(list))
	for _, p := range list {
		info := ui.ProfileInfo{ID: p.ID, Name: p.Name, Connected: connected[p.ID]}
		if db := dbs[p.ID]; db != nil {
			n, err := db.UnreadCount()
			if err != nil {
				a.log.Errorf("profile %s: unread count: %v", p.Name, err)
			}
			info.Unread = n
		}
		out = append(out, info)
	}
	return out
}

// ProfileDB returns the database of profile id, or nil if there is none.
func (a *App) ProfileDB(id string) *database.DB {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dbs[id]
}

// syncDevice syncs contacts, backfills history on first sight, then polls only
// for messages newer than the highest imported _id. Less often it re-syncs
// contacts and checks for messages deleted from the phone. It waits on
// tickers and a poke channel — never busy-waits.
func (a *App) syncDevice(ctx context.Context, d *deviceSync) {
	serial, poke := d.serial, d.poke
	// A device never imported before gets a silent historical backfill.
	_, seen, err := d.db.MaxAndroidID(serial)
	if err != nil {
		a.log.Errorf("device %s: reading watermark: %v", serial, err)
	}
	backfill := !seen

	// Calls have their own backfill: a database from before call support
	// already knows the phone but has none of its calls, and alerting on its
	// whole call log would be no better than on its whole SMS history.
	_, seenCalls, err := d.db.MaxCallID(serial)
	if err != nil {
		a.log.Errorf("device %s: reading call watermark: %v", serial, err)
	}
	cw := &callWatch{backfill: !seenCalls}

	a.syncContacts(ctx, d)
	a.pollOnce(ctx, d, &backfill)
	a.syncCalls(ctx, d, cw)
	a.checkDeleted(ctx, d)
	defer cw.drop(a.note)

	smsTick := time.NewTicker(time.Duration(a.cfg.SMSPollSeconds) * time.Second)
	defer smsTick.Stop()
	contactsTick := time.NewTicker(time.Duration(a.cfg.ContactsRefreshMinutes) * time.Minute)
	defer contactsTick.Stop()
	deletedTick := time.NewTicker(time.Duration(a.cfg.DeletedCheckMinutes) * time.Minute)
	defer deletedTick.Stop()
	callLogTick := time.NewTicker(callLogFallback)
	defer callLogTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-smsTick.C:
			a.pollOnce(ctx, d, &backfill)
			a.watchCalls(ctx, d, cw)
		case <-poke:
			a.pollOnce(ctx, d, &backfill)
			a.syncCalls(ctx, d, cw)
		case <-callLogTick.C:
			a.syncCalls(ctx, d, cw)
		case <-contactsTick.C:
			a.syncContacts(ctx, d)
		case <-deletedTick.C:
			a.checkDeleted(ctx, d)
		}
	}
}

// pollOnce imports messages newer than the current watermark for serial.
func (a *App) pollOnce(ctx context.Context, d *deviceSync, backfill *bool) {
	serial := d.serial
	sinceID, _, err := d.db.MaxAndroidID(serial)
	if err != nil {
		a.log.Errorf("device %s: watermark: %v", serial, err)
		return
	}
	msgs, err := a.adb.QuerySMS(ctx, d.conn, sinceID)
	if err != nil {
		if ctx.Err() != nil {
			return // cancelled: device unplugged, not a real error
		}
		a.log.Warnf("device %s: querying sms: %v", serial, err)
		a.mu.Lock()
		a.adbError = true
		a.mu.Unlock()
		a.refreshState()
		return
	}

	res, err := d.db.ImportMessages(serial, msgs, *backfill)
	if err != nil {
		a.log.Errorf("device %s: importing: %v", serial, err)
		return
	}
	if res.Inserted > 0 {
		a.log.Infof("device %s: imported %d message(s)%s", serial, res.Inserted,
			backfillNote(*backfill))
	}
	if *backfill {
		*backfill = false // subsequent polls notify normally
	}

	a.notifyReceived(d.db, res.NewlyReceived)
	a.settlePending(ctx, d)
	a.refreshState()
}

// settlePending re-reads outgoing messages that were imported before the
// phone finished sending them, so they do not stay "sending" forever.
func (a *App) settlePending(ctx context.Context, d *deviceSync) {
	serial := d.serial
	ids, err := d.db.PendingOutgoing(serial)
	if err != nil || len(ids) == 0 {
		if err != nil {
			a.log.Errorf("device %s: pending messages: %v", serial, err)
		}
		return
	}
	types, err := a.adb.QuerySMSTypes(ctx, d.conn, ids)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warnf("device %s: re-reading pending messages: %v", serial, err)
		}
		return
	}
	if _, err := d.db.UpdateTypes(serial, types); err != nil {
		a.log.Errorf("device %s: updating pending messages: %v", serial, err)
	}
}

func backfillNote(b bool) string {
	if b {
		return " (initial backfill, silent)"
	}
	return ""
}

// notifyReceived raises desktop notifications for newly received messages,
// collapsing a large burst into a single summary.
func (a *App) notifyReceived(db *database.DB, msgs []sms.Message) {
	if len(msgs) == 0 {
		return
	}
	if len(msgs) > 3 {
		a.note.Notify("smsd", fmt.Sprintf("%d new messages", len(msgs)))
		return
	}
	for _, m := range msgs {
		title := m.Address
		if name, err := db.ContactName(m.Address); err == nil && name != "" {
			title = name
		}
		a.note.Notify(title, truncate(m.Body, 200))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// syncContacts mirrors the device's contacts into the database, logging but
// not failing on error (contacts are best-effort).
func (a *App) syncContacts(ctx context.Context, d *deviceSync) {
	serial := d.serial
	contacts, details, err := a.adb.QueryContacts(ctx, d.conn, d.user)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warnf("device %s: querying contacts: %v", serial, err)
		}
		return
	}
	// An empty read is far more likely a provider hiccup than a phone whose
	// every contact was just deleted, and syncing it would mark them all
	// deleted.
	if len(contacts) == 0 {
		a.log.Warnf("device %s: phone returned no contacts, skipping contact sync", serial)
		return
	}
	res, err := d.db.SyncContacts(serial, contacts, details)
	if err != nil {
		a.log.Errorf("device %s: syncing contacts: %v", serial, err)
		return
	}
	a.log.Infof("device %s: synced %d contact(s)%s", serial, len(contacts), res)
}

// checkDeleted marks stored messages and calls that are no longer on the
// phone as deleted. It lists every id on the device, so it runs far less often
// than the incremental poll.
func (a *App) checkDeleted(ctx context.Context, d *deviceSync) {
	a.checkDeletedSMS(ctx, d)
	a.checkDeletedCalls(ctx, d)
}

func (a *App) checkDeletedSMS(ctx context.Context, d *deviceSync) {
	serial := d.serial
	ids, err := a.adb.QuerySMSIDs(ctx, d.conn)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warnf("device %s: listing sms ids: %v", serial, err)
		}
		return
	}
	// As with contacts, an empty listing is not trusted to mean "all deleted".
	if len(ids) == 0 {
		a.log.Warnf("device %s: phone returned no sms ids, skipping deletion check", serial)
		return
	}
	res, err := d.db.ReconcileMessages(serial, ids)
	if err != nil {
		a.log.Errorf("device %s: checking for deleted messages: %v", serial, err)
		return
	}
	if res.Removed > 0 || res.Restored > 0 {
		a.log.Infof("device %s: messages%s", serial, res)
	}
}

// --- sending ----------------------------------------------------------------

// ErrNoDevice is returned by Send when none of the profile's phones is
// connected.
var ErrNoDevice = errors.New("phone not connected")

// ErrSendDisabled is returned by Send when send_enabled is off.
var ErrSendDisabled = errors.New("sending is disabled in the configuration")

// SendStatus reports whether Send can currently work for profile, and if not,
// why.
func (a *App) SendStatus(profile string) error {
	if !a.cfg.SendEnabled {
		return ErrSendDisabled
	}
	if serial, _, _ := a.sendDevice(profile, ""); serial == "" {
		return ErrNoDevice
	}
	return nil
}

// Send sends body to address from one of profile's phones and returns the
// address it was sent to, with a phone number's formatting stripped (see
// sms.Dialable). A reply goes out from the phone the conversation was last
// on, when it is connected; otherwise from another connected phone of the
// profile. The sent message reaches the database through the regular import.
func (a *App) Send(ctx context.Context, profile, address, body string) (string, error) {
	if err := a.SendStatus(profile); err != nil {
		return "", err
	}
	address = sms.Dialable(address)
	if address == "" || strings.TrimSpace(body) == "" {
		return "", errors.New("recipient and message are required")
	}

	db := a.ProfileDB(profile)
	if db == nil {
		return "", fmt.Errorf("no profile %q", profile)
	}
	preferred, err := db.ConversationDevice(address)
	if err != nil {
		return "", err
	}
	serial, conn, poke := a.sendDevice(profile, preferred)
	if serial == "" {
		return "", ErrNoDevice
	}

	parts := sms.Split(body)
	if err := a.adb.SendSMS(ctx, conn, address, parts); err != nil {
		a.log.Errorf("device %s: sending to %s: %v", serial, address, err)
		return "", err
	}
	a.log.Infof("device %s: sent %d-part message to %s", serial, len(parts), address)
	select {
	case poke <- struct{}{}:
	default:
	}
	return address, nil
}

// sendDevice picks the connected device of profile to send from, preferring
// preferred, and returns its serial number, adb connection and poke channel.
// A phone never sends for another profile.
func (a *App) sendDevice(profile, preferred string) (string, string, chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ds, ok := a.devices[preferred]; ok && ds.profile == profile {
		return preferred, ds.conn, ds.poke
	}
	serials := make([]string, 0, len(a.devices))
	for s, ds := range a.devices {
		if ds.profile == profile {
			serials = append(serials, s)
		}
	}
	if len(serials) == 0 {
		return "", "", nil
	}
	sort.Strings(serials)
	ds := a.devices[serials[0]]
	return serials[0], ds.conn, ds.poke
}

// --- tray state -----------------------------------------------------------

// refreshState recomputes and applies the tray icon/tooltip from current state.
func (a *App) refreshState() {
	a.mu.Lock()
	nDev := len(a.devices)
	nPending := len(a.pending)
	adbErr := a.adbError
	a.mu.Unlock()

	unread := 0
	for _, db := range a.allDBs() {
		n, err := db.UnreadCount()
		if err != nil {
			a.log.Errorf("unread count: %v", err)
		}
		unread += n
	}

	switch {
	case nDev == 0 && adbErr:
		a.setState(tray.StateError, "adb/device error — check connection")
	case nPending > 0:
		a.setState(tray.StateUnread, "New phone connected — choose a profile for it in this menu")
	case nDev == 0:
		a.setState(tray.StateDisconnected, "No device connected")
	case unread > 0:
		a.setState(tray.StateUnread, fmt.Sprintf("%d device(s), %d unread", nDev, unread))
	case adbErr:
		a.setState(tray.StateError, "Device connected but adb reported an error")
	default:
		a.setState(tray.StateConnected, fmt.Sprintf("%d device(s) connected", nDev))
	}
}

func (a *App) setState(s tray.State, tip string) {
	if a.tray != nil {
		a.tray.SetState(s, tip)
	}
}

func (a *App) stopAllDevices() {
	a.mu.Lock()
	for serial, ds := range a.devices {
		ds.cancel()
		delete(a.devices, serial)
	}
	a.mu.Unlock()
}

// --- tray action handlers -------------------------------------------------

// OpenHistory opens the viewer in the browser.
func (a *App) OpenHistory() {
	if err := a.ui.Open(); err != nil {
		a.log.Errorf("opening viewer: %v", err)
	}
}

// Refresh triggers an immediate poll on every connected device.
func (a *App) Refresh() {
	a.mu.Lock()
	for _, ds := range a.devices {
		select {
		case ds.poke <- struct{}{}:
		default: // a poll is already pending
		}
	}
	a.mu.Unlock()
	a.log.Infof("manual refresh requested")
}

// MarkRead clears the pending-notification flag and updates the tray.
func (a *App) MarkRead() {
	for _, db := range a.allDBs() {
		if err := db.MarkAllNotified(); err != nil {
			a.log.Errorf("marking notifications read: %v", err)
		}
	}
	a.refreshState()
}

// ReadChanged updates the tray after the viewer marked messages or calls read.
func (a *App) ReadChanged() { a.refreshState() }

// Reconnect restarts the adb server and drops all device loops so they are
// re-established on the next scan.
func (a *App) Reconnect(ctx context.Context) {
	a.log.Infof("reconnect requested")
	a.stopAllDevices()
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := a.adb.KillServer(rctx); err != nil {
		a.log.Warnf("kill-server: %v", err)
	}
	if err := a.adb.StartServer(rctx); err != nil {
		a.log.Errorf("start-server: %v", err)
		a.setState(tray.StateError, "adb restart failed")
		return
	}
	a.scanDevices(ctx)
}
