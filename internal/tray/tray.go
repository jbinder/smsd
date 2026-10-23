// Package tray implements the system tray icon and menu using fyne.io/systray,
// which speaks the StatusNotifierItem D-Bus protocol (no cgo, no GTK). The icon
// reflects four daemon states and the menu exposes the daemon actions.
package tray

import (
	"sync"

	"fyne.io/systray"
)

// State is the visual state of the tray icon.
type State int

const (
	StateDisconnected State = iota // no device attached
	StateConnected                 // device attached, no unread
	StateUnread                    // unread received messages present
	StateError                     // adb/device error
)

// Callbacks are invoked (each on its own goroutine) when the user selects a
// menu item. Any may be nil.
type Callbacks struct {
	OnOpenHistory func()
	OnRefresh     func()
	OnMarkRead    func()
	OnReconnect   func()
	OnQuit        func()

	// A phone waiting for a profile is put in a new one, added to an existing
	// one, or ignored.
	OnNewProfile func(serial string)
	OnAttach     func(serial, profileID string)
	OnIgnore     func(serial string)
}

// Pending is a connected phone that belongs to no profile yet.
type Pending struct {
	Serial string
	Label  string // e.g. "XQ_BT52 (HQ617A1F6F)"
}

// Choice is an existing profile a pending phone can be added to.
type Choice struct {
	ID, Name string
}

// Tray owns the systray lifecycle.
type Tray struct {
	cb    Callbacks
	icons map[State][]byte

	mu         sync.Mutex
	ready      bool
	pending    State
	pendingTip string
	phones     []Pending
	choices    []Choice
}

// New builds a Tray with the given callbacks and pre-rendered state icons.
func New(cb Callbacks) *Tray {
	return &Tray{
		cb:      cb,
		icons:   buildIcons(),
		pending: StateDisconnected,
	}
}

// Run starts the tray event loop. It blocks until Quit is called and MUST be
// invoked from the program's main goroutine.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

// Quit tears down the tray, unblocking Run.
func (t *Tray) Quit() { systray.Quit() }

func (t *Tray) onReady() {
	systray.SetTitle("smsd")
	systray.SetTooltip("smsd")

	// Apply whatever was requested before the tray became ready.
	t.mu.Lock()
	t.ready = true
	state, tip := t.pending, t.pendingTip
	t.buildMenu()
	t.mu.Unlock()
	t.apply(state, tip)
}

func (t *Tray) onExit() {
	if t.cb.OnQuit != nil {
		t.cb.OnQuit()
	}
}

// SetPending lists the phones waiting for a profile at the top of the menu,
// each offering choices to place it in.
func (t *Tray) SetPending(phones []Pending, choices []Choice) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phones, t.choices = phones, choices
	if t.ready {
		t.buildMenu()
	}
}

// buildMenu replaces the whole menu: systray can only append items, and the
// pending phones belong above the regular actions. Caller holds mu.
func (t *Tray) buildMenu() {
	// As in apply, a vanished D-Bus connection at shutdown is not worth a panic.
	defer func() { _ = recover() }()
	systray.ResetMenu()

	for _, p := range t.phones {
		serial := p.Serial
		m := systray.AddMenuItem("New phone: "+p.Label, "Not synced until you choose a profile")
		on(m.AddSubMenuItem("Create a new profile", "Keep this phone's data in a profile of its own"),
			func() { invokeSerial(t.cb.OnNewProfile, serial) })
		for _, c := range t.choices {
			id := c.ID
			on(m.AddSubMenuItem("Add to profile "+c.Name, "Use this phone with an existing profile"),
				func() {
					if t.cb.OnAttach != nil {
						t.cb.OnAttach(serial, id)
					}
				})
		}
		on(m.AddSubMenuItem("Ignore this phone", "Never sync it"),
			func() { invokeSerial(t.cb.OnIgnore, serial) })
	}
	if len(t.phones) > 0 {
		systray.AddSeparator()
	}

	on(systray.AddMenuItem("Open SMS History", "Open the conversation viewer"), t.cb.OnOpenHistory)
	on(systray.AddMenuItem("Refresh", "Poll the device now"), t.cb.OnRefresh)
	on(systray.AddMenuItem("Mark notifications read", "Clear unread indicator"), t.cb.OnMarkRead)
	on(systray.AddMenuItem("Reconnect", "Restart adb and reconnect devices"), t.cb.OnReconnect)
	systray.AddSeparator()
	on(systray.AddMenuItem("Quit", "Stop smsd"), systray.Quit)
}

// on runs fn, on its own goroutine so a slow action (e.g. reconnect) never
// freezes the menu, each time item is clicked. It stops when a menu rebuild
// removes the item, which closes its channel.
func on(item *systray.MenuItem, fn func()) {
	go func() {
		for range item.ClickedCh {
			go invoke(fn)
		}
	}()
}

func invoke(fn func()) {
	if fn != nil {
		fn()
	}
}

func invokeSerial(fn func(string), serial string) {
	if fn != nil {
		fn(serial)
	}
}

// SetState updates the icon and tooltip. It is safe to call from any goroutine
// and before the tray is ready (the latest request is applied on ready).
func (t *Tray) SetState(state State, tooltip string) {
	t.mu.Lock()
	t.pending, t.pendingTip = state, tooltip
	ready := t.ready
	t.mu.Unlock()
	if ready {
		t.apply(state, tooltip)
	}
}

func (t *Tray) apply(state State, tooltip string) {
	// systray panics rather than erroring once its D-Bus connection is gone,
	// which happens whenever a poll races the tray teardown at shutdown. A
	// lost icon update at that point is nothing worth crashing over.
	defer func() { _ = recover() }()
	if icon, ok := t.icons[state]; ok {
		systray.SetIcon(icon)
	}
	if tooltip != "" {
		systray.SetTooltip(tooltip)
	}
}
