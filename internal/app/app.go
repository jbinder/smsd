// Package app is the daemon core: it wires the adb client, database, notifier,
// tray and viewer together. It monitors device connect/disconnect events and,
// while a device is connected, incrementally imports new SMS with no busy
// waiting and clean per-device cancellation.
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
	"github.com/jbinder/smsd/internal/sms"
	"github.com/jbinder/smsd/internal/tray"
	"github.com/jbinder/smsd/internal/ui"
)

// App holds the daemon's long-lived collaborators and per-device state.
type App struct {
	cfg  config.Config
	log  *logging.Logger
	adb  *adb.Client
	db   *database.DB
	note *notify.Notifier
	ui   *ui.Server
	tray *tray.Tray

	mu       sync.Mutex
	devices  map[string]*deviceSync // active per-device sync loops
	adbError bool                   // last device-list poll failed
	wg       sync.WaitGroup
}

// deviceSync tracks one connected device's sync goroutine.
type deviceSync struct {
	cancel context.CancelFunc
	poke   chan struct{} // buffered: a manual Refresh triggers an immediate poll
}

// New constructs an App. The tray is attached separately via SetTray because
// its lifecycle is owned by main (it must run on the main goroutine).
func New(cfg config.Config, log *logging.Logger, client *adb.Client, db *database.DB, note *notify.Notifier, viewer *ui.Server) *App {
	return &App{
		cfg:     cfg,
		log:     log,
		adb:     client,
		db:      db,
		note:    note,
		ui:      viewer,
		devices: make(map[string]*deviceSync),
	}
}

// SetTray attaches the tray so the App can reflect state changes on it.
func (a *App) SetTray(t *tray.Tray) { a.tray = t }

// Run starts the adb server and the device-monitor loop, blocking until ctx is
// cancelled. It returns after all per-device goroutines have stopped.
func (a *App) Run(ctx context.Context) {
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

	connected := map[string]bool{}
	unusable := false
	for _, d := range devs {
		if d.Connected() {
			connected[d.Serial] = true
		} else {
			// e.g. "unauthorized" or "offline": surfaced as an error state.
			unusable = true
			a.log.Warnf("device %s is %s", d.Serial, d.State)
		}
	}

	a.mu.Lock()
	a.adbError = unusable
	// Start loops for newly connected devices.
	for serial := range connected {
		if _, ok := a.devices[serial]; !ok {
			a.startDevice(ctx, serial)
		}
	}
	// Cancel loops for devices that went away.
	for serial, ds := range a.devices {
		if !connected[serial] {
			a.log.Infof("device %s disconnected", serial)
			ds.cancel()
			delete(a.devices, serial)
		}
	}
	a.mu.Unlock()

	a.refreshState()
}

// startDevice launches a sync goroutine for serial. Caller must hold a.mu.
func (a *App) startDevice(parent context.Context, serial string) {
	ctx, cancel := context.WithCancel(parent)
	ds := &deviceSync{cancel: cancel, poke: make(chan struct{}, 1)}
	a.devices[serial] = ds
	a.log.Infof("device %s connected, starting sync", serial)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.syncDevice(ctx, serial, ds.poke)
	}()
}

// syncDevice syncs contacts, backfills history on first sight, then polls only
// for messages newer than the highest imported _id. Less often it re-syncs
// contacts and checks for messages deleted from the phone. It waits on
// tickers and a poke channel — never busy-waits.
func (a *App) syncDevice(ctx context.Context, serial string, poke <-chan struct{}) {
	// A device never imported before gets a silent historical backfill.
	_, seen, err := a.db.MaxAndroidID(serial)
	if err != nil {
		a.log.Errorf("device %s: reading watermark: %v", serial, err)
	}
	backfill := !seen

	// Calls have their own backfill: a database from before call support
	// already knows the phone but has none of its calls, and alerting on its
	// whole call log would be no better than on its whole SMS history.
	_, seenCalls, err := a.db.MaxCallID(serial)
	if err != nil {
		a.log.Errorf("device %s: reading call watermark: %v", serial, err)
	}
	cw := &callWatch{backfill: !seenCalls}

	a.syncContacts(ctx, serial)
	a.pollOnce(ctx, serial, &backfill)
	a.syncCalls(ctx, serial, cw)
	a.checkDeleted(ctx, serial)
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
			a.pollOnce(ctx, serial, &backfill)
			a.watchCalls(ctx, serial, cw)
		case <-poke:
			a.pollOnce(ctx, serial, &backfill)
			a.syncCalls(ctx, serial, cw)
		case <-callLogTick.C:
			a.syncCalls(ctx, serial, cw)
		case <-contactsTick.C:
			a.syncContacts(ctx, serial)
		case <-deletedTick.C:
			a.checkDeleted(ctx, serial)
		}
	}
}

// pollOnce imports messages newer than the current watermark for serial.
func (a *App) pollOnce(ctx context.Context, serial string, backfill *bool) {
	sinceID, _, err := a.db.MaxAndroidID(serial)
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

	res, err := a.db.ImportMessages(serial, msgs, *backfill)
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

	a.notifyReceived(res.NewlyReceived)
	a.settlePending(ctx, serial)
	a.refreshState()
}

// settlePending re-reads outgoing messages that were imported before the
// phone finished sending them, so they do not stay "sending" forever.
func (a *App) settlePending(ctx context.Context, serial string) {
	ids, err := a.db.PendingOutgoing(serial)
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
	if _, err := a.db.UpdateTypes(serial, types); err != nil {
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
func (a *App) notifyReceived(msgs []sms.Message) {
	if len(msgs) == 0 {
		return
	}
	if len(msgs) > 3 {
		a.note.Notify("smsd", fmt.Sprintf("%d new messages", len(msgs)))
		return
	}
	for _, m := range msgs {
		title := m.Address
		if name, err := a.db.ContactName(m.Address); err == nil && name != "" {
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
func (a *App) syncContacts(ctx context.Context, serial string) {
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
	res, err := a.db.SyncContacts(serial, contacts, details)
	if err != nil {
		a.log.Errorf("device %s: syncing contacts: %v", serial, err)
		return
	}
	a.log.Infof("device %s: synced %d contact(s)%s", serial, len(contacts), res)
}

// checkDeleted marks stored messages and calls that are no longer on the
// phone as deleted. It lists every id on the device, so it runs far less often
// than the incremental poll.
func (a *App) checkDeleted(ctx context.Context, serial string) {
	a.checkDeletedSMS(ctx, serial)
	a.checkDeletedCalls(ctx, serial)
}

func (a *App) checkDeletedSMS(ctx context.Context, serial string) {
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
	res, err := a.db.ReconcileMessages(serial, ids)
	if err != nil {
		a.log.Errorf("device %s: checking for deleted messages: %v", serial, err)
		return
	}
	if res.Removed > 0 || res.Restored > 0 {
		a.log.Infof("device %s: messages%s", serial, res)
	}
}

// --- sending ----------------------------------------------------------------

// ErrNoDevice is returned by Send when no phone is connected.
var ErrNoDevice = errors.New("no phone connected")

// ErrSendDisabled is returned by Send when send_enabled is off.
var ErrSendDisabled = errors.New("sending is disabled in the configuration")

// SendStatus reports whether Send can currently work, and if not, why.
func (a *App) SendStatus() error {
	if !a.cfg.SendEnabled {
		return ErrSendDisabled
	}
	a.mu.Lock()
	n := len(a.devices)
	a.mu.Unlock()
	if n == 0 {
		return ErrNoDevice
	}
	return nil
}

// Send sends body to address and returns the address it was sent to, with a
// phone number's formatting stripped (see sms.Dialable). A reply goes out from
// the phone the conversation was last on, when it is connected; otherwise from
// a connected phone. The sent message reaches the database through the
// regular import.
func (a *App) Send(ctx context.Context, address, body string) (string, error) {
	if err := a.SendStatus(); err != nil {
		return "", err
	}
	address = sms.Dialable(address)
	if address == "" || strings.TrimSpace(body) == "" {
		return "", errors.New("recipient and message are required")
	}

	preferred, err := a.db.ConversationDevice(address)
	if err != nil {
		return "", err
	}
	serial, poke := a.sendDevice(preferred)
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

// sendDevice picks the connected device to send from, preferring preferred,
// and returns it with its poke channel.
func (a *App) sendDevice(preferred string) (string, chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ds, ok := a.devices[preferred]; ok {
		return preferred, ds.poke
	}
	serials := make([]string, 0, len(a.devices))
	for s := range a.devices {
		serials = append(serials, s)
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
	adbErr := a.adbError
	a.mu.Unlock()

	unread, err := a.db.UnreadCount()
	if err != nil {
		a.log.Errorf("unread count: %v", err)
	}

	switch {
	case nDev == 0 && adbErr:
		a.setState(tray.StateError, "adb/device error — check connection")
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
	if err := a.db.MarkAllNotified(); err != nil {
		a.log.Errorf("marking notifications read: %v", err)
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
