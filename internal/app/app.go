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
	devices  map[string]*deviceSync  // active per-device sync loops
	pending  map[string]adb.Device   // connected phones in no profile, awaiting a choice
	ignoring map[string]bool         // connected phones set to be ignored, logged once
	adbError bool                    // last device-list poll failed
	wg       sync.WaitGroup
}

// deviceSync tracks one connected device's sync goroutine.
type deviceSync struct {
	serial  string
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
		devices:  make(map[string]*deviceSync),
		pending:  make(map[string]adb.Device),
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
// It only performs the cheap local `adb devices` call; SMS polling happens in
// per-device goroutines and only while connected.
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

	connected := map[string]adb.Device{}
	unusable := false
	for _, d := range devs {
		if d.Connected() {
			connected[d.Serial] = d
		} else {
			// e.g. "unauthorized" or "offline": surfaced as an error state.
			unusable = true
			a.log.Warnf("device %s is %s", d.Serial, d.State)
		}
	}

	var arrived []adb.Device
	pendingChanged := false
	a.mu.Lock()
	a.adbError = unusable
	// Start loops for newly connected devices that belong to a profile; hold
	// back the rest.
	for serial, d := range connected {
		if _, ok := a.devices[serial]; ok {
			continue
		}
		if p, ok := a.profiles.ForDevice(serial); ok {
			a.startDevice(ctx, serial, p)
			continue
		}
		if a.profiles.Ignored(serial) {
			if !a.ignoring[serial] {
				a.log.Infof("device %s connected, ignored (see profiles.json)", serial)
				a.ignoring[serial] = true
			}
			continue
		}
		if _, ok := a.pending[serial]; !ok {
			a.log.Infof("device %s (%s) connected, belongs to no profile: not syncing until one is chosen", serial, d.Model)
			a.pending[serial] = d
			arrived = append(arrived, d)
			pendingChanged = true
		}
	}
	// Cancel loops for devices that went away.
	for serial, ds := range a.devices {
		if _, ok := connected[serial]; !ok {
			a.log.Infof("device %s disconnected", serial)
			ds.cancel()
			delete(a.devices, serial)
		}
	}
	for serial := range a.pending {
		if _, ok := connected[serial]; !ok {
			a.log.Infof("device %s disconnected without a profile", serial)
			delete(a.pending, serial)
			pendingChanged = true
		}
	}
	for serial := range a.ignoring {
		if _, ok := connected[serial]; !ok {
			delete(a.ignoring, serial)
		}
	}
	a.mu.Unlock()

	for _, d := range arrived {
		a.note.Notify("New phone connected",
			phoneLabel(d)+" is not synced. Choose a profile for it in the smsd tray menu.")
	}
	if pendingChanged {
		a.UpdateTrayMenu()
	}
	a.refreshState()
}

// startDevice launches a sync goroutine for serial into p's database. Caller
// must hold a.mu.
func (a *App) startDevice(parent context.Context, serial string, p profile.Profile) {
	db := a.dbs[p.ID]
	if db == nil {
		a.log.Errorf("device %s: database of profile %s is not open", serial, p.Name)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	ds := &deviceSync{serial: serial, profile: p.ID, db: db, cancel: cancel, poke: make(chan struct{}, 1)}
	a.devices[serial] = ds
	a.log.Infof("device %s connected, starting sync into profile %s", serial, p.Name)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.syncDevice(ctx, ds)
	}()
}

// phoneLabel names a phone for the user, e.g. "XQ_BT52 (HQ617A1F6F)".
func phoneLabel(d adb.Device) string {
	if d.Model == "" {
		return d.Serial
	}
	return d.Model + " (" + d.Serial + ")"
}

// --- profiles ---------------------------------------------------------------

// UpdateTrayMenu lists the phones waiting for a profile in the tray menu.
func (a *App) UpdateTrayMenu() {
	if a.tray == nil {
		return
	}
	a.mu.Lock()
	phones := make([]tray.Pending, 0, len(a.pending))
	for _, d := range a.pending {
		phones = append(phones, tray.Pending{Serial: d.Serial, Label: phoneLabel(d)})
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
func (a *App) takePending(serial string) (adb.Device, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.pending[serial]
	delete(a.pending, serial)
	return d, ok
}

// NewProfile puts a waiting phone into a new profile of its own, named after
// its model, and starts syncing it.
func (a *App) NewProfile(serial string) {
	d, ok := a.takePending(serial)
	if !ok {
		return
	}
	name := d.Model
	if name == "" {
		name = serial
	}
	p, err := a.profiles.Create(name, serial)
	if err == nil {
		_, err = a.openDB(p)
	}
	if err != nil {
		a.log.Errorf("device %s: creating profile: %v", serial, err)
		a.note.Notify("smsd", "Could not create a profile for "+phoneLabel(d)+": "+err.Error())
		a.restorePending(d)
		return
	}
	a.log.Infof("device %s: created profile %s (%s)", serial, p.Name, p.ID)
	a.adopt(serial, p)
}

// Attach adds a waiting phone to an existing profile, e.g. the replacement
// for a phone already in it, and starts syncing it.
func (a *App) Attach(serial, profileID string) {
	d, ok := a.takePending(serial)
	if !ok {
		return
	}
	p, err := a.profiles.Attach(profileID, serial)
	if err != nil {
		a.log.Errorf("device %s: adding to profile %s: %v", serial, profileID, err)
		a.note.Notify("smsd", "Could not add "+phoneLabel(d)+" to the profile: "+err.Error())
		a.restorePending(d)
		return
	}
	a.log.Infof("device %s: added to profile %s", serial, p.Name)
	a.adopt(serial, p)
}

// Ignore stops offering a waiting phone; it is never synced.
func (a *App) Ignore(serial string) {
	d, ok := a.takePending(serial)
	if !ok {
		return
	}
	if err := a.profiles.Ignore(serial); err != nil {
		a.log.Errorf("device %s: ignoring: %v", serial, err)
		a.restorePending(d)
		return
	}
	a.mu.Lock()
	a.ignoring[serial] = true
	a.mu.Unlock()
	a.log.Infof("device %s: ignored from now on", serial)
	a.UpdateTrayMenu()
	a.refreshState()
}

// restorePending puts d back among the waiting phones after a failed choice.
func (a *App) restorePending(d adb.Device) {
	a.mu.Lock()
	a.pending[d.Serial] = d
	a.mu.Unlock()
	a.UpdateTrayMenu()
}

// adopt starts syncing serial into p now that it has a profile.
func (a *App) adopt(serial string, p profile.Profile) {
	a.mu.Lock()
	if _, ok := a.devices[serial]; !ok {
		a.startDevice(a.ctx, serial, p)
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
	msgs, err := a.adb.QuerySMS(ctx, serial, sinceID)
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
	types, err := a.adb.QuerySMSTypes(ctx, serial, ids)
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
	contacts, details, err := a.adb.QueryContacts(ctx, serial)
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
	ids, err := a.adb.QuerySMSIDs(ctx, serial)
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
	if serial, _ := a.sendDevice(profile, ""); serial == "" {
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
	serial, poke := a.sendDevice(profile, preferred)
	if serial == "" {
		return "", ErrNoDevice
	}

	parts := sms.Split(body)
	if err := a.adb.SendSMS(ctx, serial, address, parts); err != nil {
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
// preferred, and returns it with its poke channel. A phone never sends for
// another profile.
func (a *App) sendDevice(profile, preferred string) (string, chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ds, ok := a.devices[preferred]; ok && ds.profile == profile {
		return preferred, ds.poke
	}
	serials := make([]string, 0, len(a.devices))
	for s, ds := range a.devices {
		if ds.profile == profile {
			serials = append(serials, s)
		}
	}
	if len(serials) == 0 {
		return "", nil
	}
	sort.Strings(serials)
	return serials[0], a.devices[serials[0]].poke
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
