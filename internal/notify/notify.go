// Package notify sends desktop notifications via libnotify's notify-send. It
// shells out rather than binding a library so the only runtime dependency is
// the standard libnotify package already present on most Arch/i3 desktops.
package notify

import (
	"context"
	"os/exec"
	"time"
)

// Notifier sends desktop notifications. A zero Notifier is usable but disabled
// until Enabled is set; use New to construct one.
type Notifier struct {
	enabled bool
	bin     string // resolved notify-send path, "" if unavailable
}

// New returns a Notifier. If enabled is true but notify-send is not on PATH,
// notifications are silently skipped so the daemon still runs headless.
func New(enabled bool) *Notifier {
	n := &Notifier{enabled: enabled}
	if path, err := exec.LookPath("notify-send"); err == nil {
		n.bin = path
	}
	return n
}

// Available reports whether notifications can actually be delivered.
func (n *Notifier) Available() bool { return n.bin != "" }

// SetEnabled toggles notification delivery at runtime.
func (n *Notifier) SetEnabled(v bool) { n.enabled = v }

// Notify sends a single notification. It is a no-op when disabled or when
// notify-send is unavailable. Errors are intentionally swallowed: a failed
// desktop notification must never disrupt SMS importing.
func (n *Notifier) Notify(title, body string) {
	if !n.enabled || n.bin == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// --app-name groups notifications; -u normal for received messages.
	cmd := exec.CommandContext(ctx, n.bin,
		"--app-name=smsd", "-u", "normal", "-i", "phone", title, body)
	_ = cmd.Run()
}
