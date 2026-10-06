// Package notify sends desktop notifications via libnotify's notify-send. It
// shells out rather than binding a library so the only runtime dependency is
// the standard libnotify package already present on most Arch/i3 desktops.
package notify

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Notifier sends desktop notifications. A zero Notifier is usable but disabled
// until Enabled is set; use New to construct one.
type Notifier struct {
	enabled    bool
	bin        string // resolved notify-send path, "" if unavailable
	gdbus      string // resolved gdbus path for closing notifications, "" if unavailable
	timeoutSec int    // seconds on screen; negative means never expire
}

// New returns a Notifier. If enabled is true but notify-send is not on PATH,
// notifications are silently skipped so the daemon still runs headless.
// timeoutSec is how long a notification stays on screen; a negative value
// makes it stay until dismissed. Some notification daemons (notably GNOME
// Shell) ignore the timeout hint entirely.
func New(enabled bool, timeoutSec int) *Notifier {
	n := &Notifier{enabled: enabled, timeoutSec: timeoutSec}
	if path, err := exec.LookPath("notify-send"); err == nil {
		n.bin = path
	}
	if path, err := exec.LookPath("gdbus"); err == nil {
		n.gdbus = path
	}
	return n
}

// Available reports whether notifications can actually be delivered.
func (n *Notifier) Available() bool { return n.bin != "" }

// SetEnabled toggles notification delivery at runtime.
func (n *Notifier) SetEnabled(v bool) { n.enabled = v }

// Options adjust a single notification.
type Options struct {
	// Icon is a freedesktop icon name; empty means "phone".
	Icon string
	// Sticky keeps the notification up until it is closed or replaced,
	// whatever the configured timeout.
	Sticky bool
	// Replace is the id of an earlier notification to update in place.
	Replace uint32
}

// Notify sends a single notification. It is a no-op when disabled or when
// notify-send is unavailable. Errors are intentionally swallowed: a failed
// desktop notification must never disrupt SMS importing.
func (n *Notifier) Notify(title, body string) {
	n.Show(title, body, Options{})
}

// Show sends a notification and returns its id, which Replace and Close take;
// 0 when nothing was shown. Like Notify, it never fails loudly.
func (n *Notifier) Show(title, body string, opt Options) uint32 {
	if !n.enabled || n.bin == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, n.bin, n.args(title, body, opt)...).Output()
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 32)
	return uint32(id)
}

// Close removes a notification shown earlier, such as an incoming call that
// was answered. Without gdbus it is replaced by one that expires at once.
func (n *Notifier) Close(id uint32) {
	if id == 0 || n.bin == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n.gdbus != "" {
		err := exec.CommandContext(ctx, n.gdbus, "call", "--session",
			"--dest", "org.freedesktop.Notifications",
			"--object-path", "/org/freedesktop/Notifications",
			"--method", "org.freedesktop.Notifications.CloseNotification",
			strconv.FormatUint(uint64(id), 10)).Run()
		if err == nil {
			return
		}
	}
	_ = exec.CommandContext(ctx, n.bin, "--app-name=smsd", "-r", strconv.FormatUint(uint64(id), 10),
		"-t", "1", "smsd", "").Run()
}

// args builds the notify-send argument list. --app-name groups notifications;
// -u normal for received messages; -t is in milliseconds, where 0 means the
// notification never expires. -p prints the id for Replace and Close.
func (n *Notifier) args(title, body string, opt Options) []string {
	ms := n.timeoutSec * 1000
	if n.timeoutSec < 0 || opt.Sticky {
		ms = 0
	}
	icon := opt.Icon
	if icon == "" {
		icon = "phone"
	}
	args := []string{"--app-name=smsd", "-u", "normal", "-i", icon, "-t", strconv.Itoa(ms), "-p"}
	if opt.Replace != 0 {
		args = append(args, "-r", strconv.FormatUint(uint64(opt.Replace), 10))
	}
	return append(args, title, body)
}
