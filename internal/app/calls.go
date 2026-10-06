package app

import (
	"context"
	"fmt"
	"time"

	"github.com/jbinder/smsd/internal/call"
	"github.com/jbinder/smsd/internal/notify"
)

// callLogFallback is how often the call log is re-read when no call was seen
// to end. Reading it starts a Java process on the phone (over a second), so
// it is not done on every poll: the cheap call-state check below triggers a
// read as soon as a call ends, and this only catches calls too short to be
// seen ringing.
const callLogFallback = time.Minute

// callLogRereads is how many polls in a row the call log is re-read after a
// call ends. Android writes the entry just after hanging up, which can be a
// moment after the call state is already idle.
const callLogRereads = 3

// callWatch is one device's view of its phone calls between polls.
type callWatch struct {
	backfill bool // next call-log import is the silent historical one

	state  int    // call.State* at the last poll
	number string // caller ringing at the last poll

	// ringNote is the "Incoming call" notification. When the call ends
	// unanswered it is turned into the missed-call alert, so the two do not
	// stack up.
	ringNote uint32
	rereads  int // call-log reads still due after a call ended

	stateErr bool // a failed state read was logged; quiet until it recovers
}

// drop clears a lingering incoming-call notification, e.g. when the phone is
// unplugged mid-ring.
func (w *callWatch) drop(n *notify.Notifier) {
	n.Close(w.ringNote)
	w.ringNote = 0
}

// watchCalls polls the phone's call state: a ringing phone raises an
// "Incoming call" notification, and the end of any call triggers a call-log
// read so missed calls are alerted on within a few seconds.
func (a *App) watchCalls(ctx context.Context, serial string, w *callWatch) {
	st, err := a.adb.CallState(ctx, serial)
	if err != nil {
		if ctx.Err() == nil && !w.stateErr {
			a.log.Warnf("device %s: reading call state (incoming-call alerts paused): %v", serial, err)
			w.stateErr = true
		}
	} else {
		if w.stateErr {
			a.log.Infof("device %s: call state readable again", serial)
			w.stateErr = false
		}
		a.callStateChanged(serial, w, st)
	}

	if w.rereads > 0 {
		w.rereads--
		a.syncCalls(ctx, serial, w)
		// Nothing was missed: the call was answered or rejected.
		if w.rereads == 0 && w.ringNote != 0 && w.state != call.StateRinging {
			w.drop(a.note)
		}
	}
}

func (a *App) callStateChanged(serial string, w *callWatch, st call.State) {
	prev := w.state
	w.state = st.State
	switch {
	case st.Ringing() && (prev != call.StateRinging || st.Number != w.number):
		w.number = st.Number
		a.log.Infof("device %s: incoming call from %s", serial, a.callerName(st.Number, call.PresentationUnknown))
		w.ringNote = a.note.Show("Incoming call", a.callerLine(st.Number),
			notify.Options{Icon: "call-start", Sticky: true, Replace: w.ringNote})
	case !st.Ringing() && prev == call.StateRinging:
		w.number = ""
		w.rereads = callLogRereads
		if st.State == call.StateOffhook {
			// Answered. The log is still read, in case this was a second
			// call that rang out during the first.
			w.drop(a.note)
		}
	case st.State == call.StateIdle && prev == call.StateOffhook:
		w.rereads = callLogRereads // a finished call; show it in the viewer promptly
	}
}

// syncCalls imports call-log entries newer than the stored ones and alerts on
// missed calls among them.
func (a *App) syncCalls(ctx context.Context, serial string, w *callWatch) {
	sinceID, _, err := a.db.MaxCallID(serial)
	if err != nil {
		a.log.Errorf("device %s: call watermark: %v", serial, err)
		return
	}
	calls, err := a.adb.QueryCalls(ctx, serial, sinceID)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warnf("device %s: querying call log: %v", serial, err)
		}
		return
	}
	res, err := a.db.ImportCalls(serial, calls, w.backfill)
	if err != nil {
		a.log.Errorf("device %s: importing calls: %v", serial, err)
		return
	}
	if res.Inserted > 0 {
		a.log.Infof("device %s: imported %d call(s)%s", serial, res.Inserted, backfillNote(w.backfill))
	}
	w.backfill = false

	if len(res.NewlyMissed) > 0 {
		a.notifyMissed(res.NewlyMissed, w)
		w.rereads = 0
	}
	a.refreshState()
}

// notifyMissed alerts on missed calls. The first replaces a still-showing
// "Incoming call" notification; a burst of more than three is summarised.
func (a *App) notifyMissed(calls []call.Call, w *callWatch) {
	opt := notify.Options{Icon: "call-missed"}
	// A call ringing right now keeps its notification.
	if w.state != call.StateRinging {
		opt.Replace = w.ringNote
		w.ringNote = 0
	}
	if len(calls) > 3 {
		a.note.Show("smsd", fmt.Sprintf("%d missed calls", len(calls)), opt)
		return
	}
	for _, c := range calls {
		a.log.Infof("missed call from %s", a.callerName(c.Number, c.Presentation))
		body := a.callerLine(c.Number)
		if c.Number == "" {
			body = call.PresentationLabel(c.Presentation)
		}
		// One seen ringing just now needs no time; one found later does.
		if time.Since(time.UnixMilli(c.Date)) > 5*time.Minute {
			body += "\n" + time.UnixMilli(c.Date).Format("Mon 2 Jan 15:04")
		}
		a.note.Show("Missed call", body, opt)
		opt.Replace = 0
	}
}

// callerName is the contact name for number, or the number, or why there is
// none.
func (a *App) callerName(number string, presentation int) string {
	if number == "" {
		return call.PresentationLabel(presentation)
	}
	if name, err := a.db.ContactName(number); err == nil && name != "" {
		return name
	}
	return number
}

// callerLine is a notification body naming the caller, with the number
// alongside a contact name.
func (a *App) callerLine(number string) string {
	if number == "" {
		return "Unknown number"
	}
	if name, err := a.db.ContactName(number); err == nil && name != "" {
		return name + "\n" + number
	}
	return number
}

// checkDeletedCalls marks stored calls cleared from the phone's call log.
func (a *App) checkDeletedCalls(ctx context.Context, serial string) {
	ids, err := a.adb.QueryCallIDs(ctx, serial)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warnf("device %s: listing call ids: %v", serial, err)
		}
		return
	}
	// As with messages, an empty listing is not trusted to mean "all deleted".
	if len(ids) == 0 {
		a.log.Warnf("device %s: phone returned no call ids, skipping deletion check", serial)
		return
	}
	res, err := a.db.ReconcileCalls(serial, ids)
	if err != nil {
		a.log.Errorf("device %s: checking for deleted calls: %v", serial, err)
		return
	}
	if res.Removed > 0 || res.Restored > 0 {
		a.log.Infof("device %s: calls%s", serial, res)
	}
}
