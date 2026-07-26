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
}

// Tray owns the systray lifecycle.
type Tray struct {
	cb    Callbacks
	icons map[State][]byte

	mu         sync.Mutex
	ready      bool
	pending    State
	pendingTip string

	mOpen    *systray.MenuItem
	mRefresh *systray.MenuItem
	mMark    *systray.MenuItem
	mRecon   *systray.MenuItem
	mQuit    *systray.MenuItem
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

	t.mOpen = systray.AddMenuItem("Open SMS History", "Open the conversation viewer")
	t.mRefresh = systray.AddMenuItem("Refresh", "Poll the device now")
	t.mMark = systray.AddMenuItem("Mark notifications read", "Clear unread indicator")
	t.mRecon = systray.AddMenuItem("Reconnect", "Restart adb and reconnect devices")
	systray.AddSeparator()
	t.mQuit = systray.AddMenuItem("Quit", "Stop smsd")

	// Apply whatever state was requested before the tray became ready.
	t.mu.Lock()
	t.ready = true
	state, tip := t.pending, t.pendingTip
	t.mu.Unlock()
	t.apply(state, tip)

	go t.loop()
}

func (t *Tray) onExit() {
	if t.cb.OnQuit != nil {
		t.cb.OnQuit()
	}
}

// loop dispatches menu clicks. Each handler runs on its own goroutine so a slow
// action (e.g. reconnect) never freezes the menu.
func (t *Tray) loop() {
	for {
		select {
		case <-t.mOpen.ClickedCh:
			go invoke(t.cb.OnOpenHistory)
		case <-t.mRefresh.ClickedCh:
			go invoke(t.cb.OnRefresh)
		case <-t.mMark.ClickedCh:
			go invoke(t.cb.OnMarkRead)
		case <-t.mRecon.ClickedCh:
			go invoke(t.cb.OnReconnect)
		case <-t.mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func invoke(fn func()) {
	if fn != nil {
		fn()
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
	if icon, ok := t.icons[state]; ok {
		systray.SetIcon(icon)
	}
	if tooltip != "" {
		systray.SetTooltip(tooltip)
	}
}
