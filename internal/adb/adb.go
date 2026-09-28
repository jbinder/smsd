// Package adb wraps the adb command-line tool. It only uses stable, non-root
// adb shell commands (`content query` against the standard providers) so it
// needs no companion app on the device. All calls are context-aware so the
// daemon can cancel work when a device disconnects or shuts down.
package adb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/jbinder/smsd/internal/contact"
	"github.com/jbinder/smsd/internal/sms"
)

// Client runs adb commands using a configured adb binary path.
type Client struct {
	path string
}

// New returns a Client. If path is empty, "adb" is resolved from PATH.
func New(path string) *Client {
	if path == "" {
		path = "adb"
	}
	return &Client{path: path}
}

// Device is an entry from `adb devices`.
type Device struct {
	Serial string
	State  string // "device", "unauthorized", "offline", ...
}

// Connected reports whether the device is fully usable.
func (d Device) Connected() bool { return d.State == "device" }

// commandTimeout bounds any single adb invocation so a wedged USB link cannot
// block a goroutine forever.
const commandTimeout = 30 * time.Second

// run executes adb with args and returns stdout. Stderr is folded into the
// error so callers can log a useful message.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("adb %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// StartServer ensures the adb server is running. It is safe to call repeatedly.
func (c *Client) StartServer(ctx context.Context) error {
	_, err := c.run(ctx, "start-server")
	return err
}

// KillServer stops the adb server. Used by the Reconnect action to force a
// clean re-detection of attached devices.
func (c *Client) KillServer(ctx context.Context) error {
	_, err := c.run(ctx, "kill-server")
	return err
}

// Devices returns the currently known devices and their states.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	out, err := c.run(ctx, "devices")
	if err != nil {
		return nil, err
	}
	return parseDevices(out), nil
}

func parseDevices(out string) []Device {
	var devs []Device
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		devs = append(devs, Device{Serial: fields[0], State: fields[1]})
	}
	return devs
}

// GetState returns the state of a single device ("device" when ready).
func (c *Client) GetState(ctx context.Context, serial string) (string, error) {
	out, err := c.run(ctx, "-s", serial, "get-state")
	return strings.TrimSpace(out), err
}

// ErrNoRows is returned by QuerySMS when the provider reports no matching rows.
var ErrNoRows = errors.New("no rows")

// QuerySMS reads messages from content://sms for a device. When sinceID > 0
// only rows with a larger _id are requested, so steady-state polling transfers
// just the handful of new messages regardless of how many are stored.
func (c *Client) QuerySMS(ctx context.Context, serial string, sinceID int64) ([]sms.Message, error) {
	remote := fmt.Sprintf(
		"content query --uri content://sms --projection %s --sort '_id ASC'",
		sms.ProjectionArg())
	if sinceID > 0 {
		// Single quotes protect '>' from the device-side shell.
		remote += fmt.Sprintf(" --where '_id>%d'", sinceID)
	}
	out, err := c.run(ctx, "-s", serial, "shell", remote)
	if err != nil {
		return nil, err
	}
	return sms.ParseQueryOutput(out), nil
}

// QuerySMSIDs lists the _id of every message on the device. It is the cheap
// half of noticing deletions: comparing it with the stored ids shows which
// messages are gone from the phone without transferring any bodies.
func (c *Client) QuerySMSIDs(ctx context.Context, serial string) ([]int64, error) {
	out, err := c.run(ctx, "-s", serial, "shell", "content query --uri content://sms --projection _id")
	if err != nil {
		return nil, err
	}
	msgs := sms.ParseQueryOutput(out)
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.AndroidID
	}
	return ids, nil
}

// QueryContacts reads every contact and its details (numbers, e-mail
// addresses, …) from the Contacts Provider. Both queries must succeed: a
// partial read would make the missing half look deleted.
func (c *Client) QueryContacts(ctx context.Context, serial string) ([]contact.Contact, []contact.Detail, error) {
	out, err := c.run(ctx, "-s", serial, "shell",
		"content query --uri content://com.android.contacts/contacts --projection "+
			strings.Join(contact.ContactProjection, ":"))
	if err != nil {
		return nil, nil, err
	}
	contacts := contact.ParseContacts(out)

	out, err = c.run(ctx, "-s", serial, "shell",
		"content query --uri content://com.android.contacts/data --projection "+
			strings.Join(contact.DetailProjection, ":"))
	if err != nil {
		return nil, nil, err
	}
	return contacts, contact.ParseDetails(out), nil
}
