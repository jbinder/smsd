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
	"regexp"
	"strconv"
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

// QuerySMSTypes reads the current type of the given messages, keyed by _id.
func (c *Client) QuerySMSTypes(ctx context.Context, serial string, ids []int64) (map[int64]int, error) {
	list := make([]string, len(ids))
	for i, id := range ids {
		list[i] = strconv.FormatInt(id, 10)
	}
	out, err := c.run(ctx, "-s", serial, "shell", fmt.Sprintf(
		"content query --uri content://sms --projection _id:type --where '_id IN (%s)'",
		strings.Join(list, ",")))
	if err != nil {
		return nil, err
	}
	types := map[int64]int{}
	for _, m := range sms.ParseQueryOutput(out) {
		types[m.AndroidID] = m.Type
	}
	return types, nil
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

// ErrSendUnsupported is returned by SendSMS on Android versions whose telephony
// service layout smsd has not been verified against.
var ErrSendUnsupported = errors.New("sending SMS is not supported on this Android version")

// ismsCalls holds the binder transaction codes of the telephony ISms service,
// keyed by SDK level. They are the method's position in the AIDL interface
// and may shift between releases, so a level is only listed once its codes
// have been read from that release's framework.jar (ISms$Stub TRANSACTION_*).
var ismsCalls = map[int]struct {
	sendText, sendMultipart, preferredSub int
}{
	33: {sendText: 5, sendMultipart: 8, preferredSub: 20}, // Android 13
}

// SendSMS sends text to dest from the device's default SMS subscription.
// parts is the text as divided by sms.Split; more than one is sent as a
// concatenated message.
//
// There is no app on the phone to do this, so smsd calls the telephony
// service directly as the adb shell user, which holds SEND_SMS. Android then
// stores the message in the sent (or failed) folder itself, where the regular
// import picks it up. A nil error means the phone accepted the message, not
// that it was delivered.
func (c *Client) SendSMS(ctx context.Context, serial, dest string, parts []string) error {
	if len(parts) == 0 {
		return errors.New("empty message")
	}
	out, err := c.run(ctx, "-s", serial, "shell", "getprop ro.build.version.sdk")
	if err != nil {
		return err
	}
	sdk, _ := strconv.Atoi(strings.TrimSpace(out))
	calls, ok := ismsCalls[sdk]
	if !ok {
		return fmt.Errorf("%w (SDK %d)", ErrSendUnsupported, sdk)
	}

	out, err = c.run(ctx, "-s", serial, "shell", fmt.Sprintf("service call isms %d", calls.preferredSub))
	if err != nil {
		return err
	}
	sub, err := parseParcelInt(out)
	if err != nil {
		return fmt.Errorf("reading SMS subscription: %w", err)
	}
	if sub < 0 {
		return errors.New("the phone has no SMS subscription (no SIM?)")
	}

	out, err = c.run(ctx, "-s", serial, "shell", sendCommand(calls.sendText, calls.sendMultipart, int(sub), dest, parts))
	if err != nil {
		return err
	}
	if _, err := parseParcelInt(out); err != nil {
		return fmt.Errorf("sending: %w", err)
	}
	return nil
}

// sendCommand builds the device-side `service call` for
//
//	sendTextForSubscriber(int subId, String callingPkg, String attributionTag,
//	    String destAddr, String scAddr, String text, PendingIntent sentIntent,
//	    PendingIntent deliveryIntent, boolean persistMessage, long messageId)
//
// or, for several parts, sendMultipartTextForSubscriber, which takes
// List<String> parts and List<PendingIntent> in place of the text and intents.
// A null String is written as length -1 and a null object or list as -1 or 0,
// exactly as the AIDL proxy would. persistMessage asks Android to file the
// message in the sent folder, as it only does unasked for the default SMS app.
func sendCommand(sendText, sendMultipart, sub int, dest string, parts []string) string {
	head := fmt.Sprintf("i32 %d s16 com.android.shell i32 -1 s16 %s i32 -1", sub, shellQuote(dest))
	if len(parts) == 1 {
		return fmt.Sprintf("service call isms %d %s s16 %s i32 0 i32 0 i32 1 i64 0",
			sendText, head, shellQuote(parts[0]))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "service call isms %d %s i32 %d", sendMultipart, head, len(parts))
	for _, p := range parts {
		b.WriteString(" s16 ")
		b.WriteString(shellQuote(p))
	}
	b.WriteString(" i32 -1 i32 -1 i32 1 i64 0")
	return b.String()
}

// shellQuote quotes s for the device shell. Inside single quotes nothing is
// special, newlines included, except the single quote itself.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parcelWord matches the hex words `service call` prints for a reply parcel,
// e.g. "Result: Parcel(00000000 00000001   '........')".
var parcelWord = regexp.MustCompile(`\b[0-9a-f]{8}\b`)

// parseParcelInt reads a reply parcel: the first word is the exception code
// (0 for none), the second the int return value if there is one. On an
// exception the parcel carries the message as UTF-16 text, which `service
// call` shows in the quoted column; it is included in the error.
func parseParcelInt(out string) (int32, error) {
	start := strings.Index(out, "Parcel(")
	if start < 0 {
		return 0, fmt.Errorf("unexpected reply: %s", strings.TrimSpace(out))
	}
	body := out[start:]
	var words []string
	for _, line := range strings.Split(body, "\n") {
		// Only the hex column; the quoted column can contain hex-looking text.
		if q := strings.Index(line, "'"); q >= 0 {
			line = line[:q]
		}
		words = append(words, parcelWord.FindAllString(line, -1)...)
	}
	if len(words) == 0 {
		return 0, fmt.Errorf("unexpected reply: %s", strings.TrimSpace(out))
	}
	if words[0] != "00000000" {
		return 0, fmt.Errorf("phone refused: %s", parcelText(body))
	}
	if len(words) < 2 {
		return 0, nil
	}
	v, err := strconv.ParseUint(words[1], 16, 32)
	if err != nil {
		return 0, err
	}
	return int32(uint32(v)), nil
}

// parcelText joins the quoted text column of a `service call` dump. Strings in
// a parcel are UTF-16, which the dump shows as "S.e.c.u.r.i.t.y": every other
// byte is zero and printed as a dot, so the dots are dropped.
func parcelText(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		first := strings.Index(line, "'")
		last := strings.LastIndex(line, "'")
		if first >= 0 && last > first {
			b.WriteString(line[first+1 : last])
		}
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(b.String(), ".", "")), " ")
}
