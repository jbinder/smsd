package database

import (
	"testing"

	"github.com/jbinder/smsd/internal/call"
	"github.com/jbinder/smsd/internal/contact"
	"github.com/jbinder/smsd/internal/sms"
)

func phoneCall(id int64, typ int, number string) call.Call {
	return call.Call{
		AndroidID:    id,
		Number:       number,
		Date:         1600000000000 + id*1000,
		Type:         typ,
		Presentation: call.PresentationAllowed,
	}
}

func TestImportCalls_DedupAndMissedAlerts(t *testing.T) {
	db := openTestDB(t)
	history := []call.Call{
		phoneCall(1, call.TypeMissed, "+15551234567"),
		phoneCall(2, call.TypeIncoming, "+15551234567"),
	}
	r, err := db.ImportCalls("dev1", history, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Inserted != 2 || len(r.NewlyMissed) != 0 {
		t.Fatalf("backfill = %+v, want 2 inserted and no alerts", r)
	}
	if n, _ := db.UnreadCount(); n != 0 {
		t.Errorf("unread after backfill = %d, want 0", n)
	}

	live := append(history,
		phoneCall(3, call.TypeMissed, "+15550000000"),
		phoneCall(4, call.TypeRejected, "+15550000000"),
		phoneCall(5, call.TypeOutgoing, "+15550000000"))
	r, err = db.ImportCalls("dev1", live, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Inserted != 3 {
		t.Errorf("inserted %d, want 3 (dedup failed)", r.Inserted)
	}
	if len(r.NewlyMissed) != 1 || r.NewlyMissed[0].AndroidID != 3 {
		t.Errorf("NewlyMissed = %+v, want only id 3 (rejected is not missed)", r.NewlyMissed)
	}
	if max, ok, _ := db.MaxCallID("dev1"); !ok || max != 5 {
		t.Errorf("MaxCallID = %d, %v; want 5, true", max, ok)
	}
}

func TestUnreadCount_MessagesAndCalls(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportMessages("dev1", []sms.Message{msg(1, sms.TypeReceived, "hi")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportCalls("dev1", []call.Call{phoneCall(1, call.TypeMissed, "+1")}, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.UnreadCount(); n != 2 {
		t.Errorf("unread = %d, want 2", n)
	}
	if err := db.MarkAllNotified(); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.UnreadCount(); n != 0 {
		t.Errorf("unread after MarkAllNotified = %d, want 0", n)
	}
}

func TestReconcileCalls(t *testing.T) {
	db := openTestDB(t)
	calls := []call.Call{phoneCall(1, call.TypeIncoming, "+1"), phoneCall(2, call.TypeOutgoing, "+1")}
	if _, err := db.ImportCalls("dev1", calls, true); err != nil {
		t.Fatal(err)
	}
	res, err := db.ReconcileCalls("dev1", []int64{2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Errorf("removed = %d, want 1", res.Removed)
	}
	page, err := db.Calls("", 0, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Calls) != 2 {
		t.Fatalf("calls = %d, want 2 (nothing is ever removed)", len(page.Calls))
	}
	if page.Calls[1].AndroidID != 1 || page.Calls[1].DeletedAt == 0 {
		t.Errorf("call 1 not marked deleted: %+v", page.Calls[1])
	}
	if s, _ := db.State(); s.Calls.Count != 2 || s.Calls.Deleted != 1 {
		t.Errorf("state = %+v", s.Calls)
	}

	res, err = db.ReconcileCalls("dev1", []int64{1, 2})
	if err != nil || res.Restored != 1 {
		t.Errorf("restore = %+v, %v; want 1 restored", res, err)
	}
}

func TestCalls_PagingFilterAndNames(t *testing.T) {
	db := openTestDB(t)
	_, err := db.SyncContacts("dev1",
		[]contact.Contact{{AndroidID: 1, Name: "Alice"}},
		[]contact.Detail{{AndroidID: 10, ContactID: 1, Kind: contact.KindPhone, Value: "+1 555 123 4567"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportMessages("dev1", []sms.Message{msg(1, sms.TypeReceived, "hi")}, true); err != nil {
		t.Fatal(err)
	}
	withheld := phoneCall(4, call.TypeMissed, "")
	withheld.Presentation = call.PresentationRestricted
	calls := []call.Call{
		phoneCall(1, call.TypeIncoming, "+15551234567"),
		phoneCall(2, call.TypeOutgoing, "+15550000000"),
		phoneCall(3, call.TypeMissed, "+15551234567"),
		withheld,
	}
	if _, err := db.ImportCalls("dev1", calls, true); err != nil {
		t.Fatal(err)
	}

	page, err := db.Calls("", 0, Cursor{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || len(page.Calls) != 2 || page.Calls[0].AndroidID != 4 || page.Calls[1].AndroidID != 3 {
		t.Fatalf("first page = %+v", page)
	}
	if page.Calls[0].Caller != "Private number" {
		t.Errorf("withheld caller = %q", page.Calls[0].Caller)
	}
	alice := page.Calls[1]
	if alice.ContactName != "Alice" || alice.Caller != "Alice" || alice.Conversation != "+15551234567" {
		t.Errorf("resolved call = %+v", alice)
	}

	last := page.Calls[1]
	page, err = db.Calls("", 0, Cursor{Date: last.Date, AndroidID: last.AndroidID}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.HasMore || len(page.Calls) != 2 || page.Calls[0].AndroidID != 2 {
		t.Errorf("second page = %+v", page)
	}

	page, err = db.Calls("(555) 123-4567", 0, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Calls) != 2 {
		t.Errorf("by number = %d calls, want 2", len(page.Calls))
	}

	page, err = db.Calls("", calls[2].Date, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Calls) != 2 {
		t.Errorf("windowed = %d calls, want 2", len(page.Calls))
	}
}

func TestMarkCallsRead(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportCalls("dev1", []call.Call{
		phoneCall(1, call.TypeMissed, "+1"),
		phoneCall(2, call.TypeMissed, "+2"),
	}, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := db.State(); s.Calls.Unread != 2 {
		t.Fatalf("State.Calls.Unread = %d, want 2", s.Calls.Unread)
	}
	n, err := db.MarkCallsRead(phoneCall(1, 0, "").Date)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("marked %d, want 1", n)
	}
	page, err := db.Calls("", 0, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Calls {
		if want := c.AndroidID == 2; c.Unread != want {
			t.Errorf("call %d unread = %v, want %v", c.AndroidID, c.Unread, want)
		}
	}
}
