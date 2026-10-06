// Package call defines the phone call model, the parser for `content query`
// output from Android's call log, and the parser for the live call state in
// `dumpsys telephony.registry`. Like packages sms and contact it does no I/O,
// so the parsing can be unit-tested in isolation.
package call

import (
	"regexp"
	"strconv"
	"strings"
)

// Call type constants as used by Android's content://call_log/calls "type"
// column (CallLog.Calls.*_TYPE).
const (
	TypeIncoming         = 1
	TypeOutgoing         = 2
	TypeMissed           = 3
	TypeVoicemail        = 4
	TypeRejected         = 5
	TypeBlocked          = 6
	TypeAnsweredExternal = 7
)

// Presentation codes (CallLog.Calls.PRESENTATION_*). Anything but allowed
// means the network withheld the number and the number column is empty.
const (
	PresentationAllowed    = 1
	PresentationRestricted = 2
	PresentationUnknown    = 3
	PresentationPayphone   = 4
)

// Projection is the column list smsd requests from content://call_log/calls.
// None of these columns hold free text, so a plain "k=v, k=v" split is safe.
var Projection = []string{"_id", "number", "date", "duration", "type", "presentation"}

// ProjectionArg returns the colon-joined projection string content query wants.
func ProjectionArg() string { return strings.Join(Projection, ":") }

// Call is one call log entry as imported from a device.
type Call struct {
	AndroidID    int64  // _id on the device (unique per device)
	Number       string // as the phone recorded it; empty when withheld
	Date         int64  // start of the call, epoch milliseconds
	Duration     int64  // seconds; 0 for missed and rejected calls
	Type         int    // TypeIncoming, TypeMissed, ...
	Presentation int    // PresentationAllowed, ...
}

// Missed reports whether this is a call the user should be alerted about.
// Rejected and blocked calls were dealt with on purpose and are not.
func (c Call) Missed() bool { return c.Type == TypeMissed }

// Caller is a display name for the other party when no contact matches:
// the number, or why there is none.
func (c Call) Caller() string {
	if c.Number != "" {
		return c.Number
	}
	return PresentationLabel(c.Presentation)
}

// PresentationLabel names a withheld number the way phone dialers do.
func PresentationLabel(p int) string {
	switch p {
	case PresentationRestricted:
		return "Private number"
	case PresentationPayphone:
		return "Payphone"
	default:
		return "Unknown"
	}
}

// rowSplit matches the "Row: <n> " prefix content query puts before each record.
var rowSplit = regexp.MustCompile(`(?m)^Row: \d+ `)

// ParseQueryOutput parses the stdout of
//
//	content query --uri content://call_log/calls --projection <Projection> ...
//
// into calls. Rows without a usable _id are skipped rather than failing the
// batch. It also reads a narrower projection, such as _id alone.
func ParseQueryOutput(raw string) []Call {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "No result found") {
		return nil
	}
	var out []Call
	for _, chunk := range rowSplit.Split(raw, -1) {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		f := map[string]string{}
		for _, part := range strings.Split(chunk, ", ") {
			if eq := strings.Index(part, "="); eq >= 0 {
				v := strings.TrimSpace(part[eq+1:])
				if v == "NULL" {
					v = ""
				}
				f[strings.TrimSpace(part[:eq])] = v
			}
		}
		id, err := strconv.ParseInt(f["_id"], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, Call{
			AndroidID:    id,
			Number:       f["number"],
			Date:         parseInt(f["date"]),
			Duration:     parseInt(f["duration"]),
			Type:         int(parseInt(f["type"])),
			Presentation: int(parseInt(f["presentation"])),
		})
	}
	return out
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// Call states as TelephonyManager.CALL_STATE_* reports them.
const (
	StateIdle    = 0
	StateRinging = 1
	StateOffhook = 2 // dialling, in a call, or on hold
)

// State is the phone's call state across all its SIMs.
type State struct {
	// State is the busiest SIM's state: ringing beats offhook beats idle, as
	// a second call ringing in during a call is what the user needs to see.
	State int
	// Number is the ringing caller's number, when the phone reports it.
	Number string
}

// Ringing reports whether a call is ringing.
func (s State) Ringing() bool { return s.State == StateRinging }

// StateGrep filters `dumpsys telephony.registry` on the phone down to the
// lines ParseState reads. The full dump is several hundred kilobytes.
const StateGrep = `grep -E '^ *(Phone Id=|mCallState=|mCallIncomingNumber=)'`

// ParseState reads the per-SIM call state from `dumpsys telephony.registry`,
// filtered by StateGrep. The dump repeats mCallState for each "Phone Id"
// section; on Android 13 a section looks like
//
//	Phone Id=0
//	  mCallState=1
//	  mCallIncomingNumber=+436601489613
//
// ok is false when the dump has no call state at all, so an unreadable dump
// is not mistaken for an idle phone.
func ParseState(raw string) (st State, ok bool) {
	ringing := false // the current Phone Id section's SIM is ringing
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Phone Id="):
			ringing = false
		case strings.HasPrefix(line, "mCallState="):
			n, err := strconv.Atoi(strings.TrimPrefix(line, "mCallState="))
			if err != nil {
				continue
			}
			ok = true
			ringing = n == StateRinging
			if rank(n) > rank(st.State) {
				st.State = n
			}
		case strings.HasPrefix(line, "mCallIncomingNumber="):
			// The number belongs to the SIM whose section it is in; only a
			// ringing SIM's number is the caller.
			if num := strings.TrimPrefix(line, "mCallIncomingNumber="); ringing && num != "" && st.Number == "" {
				st.Number = num
			}
		}
	}
	return st, ok
}

// rank orders call states by how much they matter to the user.
func rank(s int) int {
	switch s {
	case StateRinging:
		return 2
	case StateOffhook:
		return 1
	}
	return 0
}
