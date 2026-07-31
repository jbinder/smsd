package database

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbinder/smsd/internal/sms"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func msg(id int64, typ int, body string) sms.Message {
	return sms.Message{
		AndroidID: id,
		ThreadID:  1,
		Address:   "+15551234567",
		Body:      body,
		Date:      1600000000000 + id,
		Type:      typ,
	}
}

func TestImport_NoDuplicates(t *testing.T) {
	db := openTestDB(t)
	batch := []sms.Message{
		msg(1, sms.TypeReceived, "a"),
		msg(2, sms.TypeSent, "b"),
	}
	// Backfill so nothing is flagged for notification.
	r1, err := db.ImportMessages("dev1", batch, true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if r1.Inserted != 2 {
		t.Fatalf("first import inserted %d, want 2", r1.Inserted)
	}

	// Re-importing the same rows plus one new row must only insert the new one.
	batch2 := append(batch, msg(3, sms.TypeReceived, "c"))
	r2, err := db.ImportMessages("dev1", batch2, false)
	if err != nil {
		t.Fatalf("import2: %v", err)
	}
	if r2.Inserted != 1 {
		t.Errorf("second import inserted %d, want 1 (dedup failed)", r2.Inserted)
	}
	if len(r2.NewlyReceived) != 1 || r2.NewlyReceived[0].AndroidID != 3 {
		t.Errorf("NewlyReceived = %+v, want id 3", r2.NewlyReceived)
	}
}

func TestImport_SameIDDifferentDevices(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportMessages("devA", []sms.Message{msg(1, sms.TypeReceived, "a")}, true); err != nil {
		t.Fatal(err)
	}
	// Same android_id on a different device is a distinct message.
	r, err := db.ImportMessages("devB", []sms.Message{msg(1, sms.TypeReceived, "a")}, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Inserted != 1 {
		t.Errorf("cross-device import inserted %d, want 1", r.Inserted)
	}
}

func TestBackfill_DoesNotNotify(t *testing.T) {
	db := openTestDB(t)
	r, err := db.ImportMessages("dev1", []sms.Message{
		msg(1, sms.TypeReceived, "old1"),
		msg(2, sms.TypeReceived, "old2"),
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.NewlyReceived) != 0 {
		t.Errorf("backfill produced %d notifications, want 0", len(r.NewlyReceived))
	}
	n, err := db.UnreadCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("UnreadCount after backfill = %d, want 0", n)
	}
}

func TestUnreadAndMarkNotified(t *testing.T) {
	db := openTestDB(t)
	// Live batch with two received and one sent.
	if _, err := db.ImportMessages("dev1", []sms.Message{
		msg(1, sms.TypeReceived, "r1"),
		msg(2, sms.TypeSent, "s1"),
		msg(3, sms.TypeReceived, "r2"),
	}, false); err != nil {
		t.Fatal(err)
	}
	n, err := db.UnreadCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("UnreadCount = %d, want 2", n)
	}
	if err := db.MarkAllNotified(); err != nil {
		t.Fatal(err)
	}
	n, _ = db.UnreadCount()
	if n != 0 {
		t.Errorf("UnreadCount after mark = %d, want 0", n)
	}
}

func TestMaxAndroidID(t *testing.T) {
	db := openTestDB(t)
	if _, ok, err := db.MaxAndroidID("dev1"); err != nil || ok {
		t.Fatalf("empty MaxAndroidID: ok=%v err=%v, want ok=false", ok, err)
	}
	if _, err := db.ImportMessages("dev1", []sms.Message{
		msg(5, sms.TypeReceived, "x"),
		msg(9, sms.TypeSent, "y"),
		msg(7, sms.TypeReceived, "z"),
	}, true); err != nil {
		t.Fatal(err)
	}
	max, ok, err := db.MaxAndroidID("dev1")
	if err != nil || !ok {
		t.Fatalf("MaxAndroidID err=%v ok=%v", err, ok)
	}
	if max != 9 {
		t.Errorf("MaxAndroidID = %d, want 9", max)
	}
}

func TestContactsAndSearch(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportMessages("dev1", []sms.Message{
		msg(1, sms.TypeReceived, "Meeting at noon"),
		msg(2, sms.TypeSent, "See you there"),
	}, true); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertContacts("dev1", []Contact{
		{Phone: "(555) 123-4567", Name: "Alice Example"},
	}); err != nil {
		t.Fatal(err)
	}

	// Name resolves from the differently-formatted stored number.
	name, err := db.ContactName("+1 555 123 4567")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Alice Example" {
		t.Errorf("ContactName = %q, want Alice Example", name)
	}

	// Search by body.
	if got, _ := db.Search("noon"); len(got) != 1 || got[0].AndroidID != 1 {
		t.Errorf("body search = %+v, want id 1", got)
	}
	// Search by contact name.
	if got, _ := db.Search("alice"); len(got) != 2 {
		t.Errorf("name search returned %d, want 2", len(got))
	}
	// Search by phone number fragment.
	if got, _ := db.Search("1234567"); len(got) != 2 {
		t.Errorf("phone search returned %d, want 2", len(got))
	}
}

func TestConversations(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportMessages("dev1", []sms.Message{
		{AndroidID: 1, Address: "+15550000001", Body: "hi", Date: 100, Type: sms.TypeReceived},
		{AndroidID: 2, Address: "+15550000001", Body: "bye", Date: 200, Type: sms.TypeSent},
		{AndroidID: 3, Address: "+15550000002", Body: "yo", Date: 150, Type: sms.TypeReceived},
	}, true); err != nil {
		t.Fatal(err)
	}
	convs, err := db.Conversations(0, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convs))
	}
	// Most recently active first: address ...0001 last date 200.
	if convs[0].Address != "+15550000001" || convs[0].LastBody != "bye" || convs[0].Count != 2 {
		t.Errorf("conv[0] = %+v", convs[0])
	}
	if convs[1].Address != "+15550000002" || convs[1].Count != 1 {
		t.Errorf("conv[1] = %+v", convs[1])
	}
}

func TestConversationsPaging(t *testing.T) {
	db := openTestDB(t)
	var msgs []sms.Message
	for i := 1; i <= 5; i++ {
		msgs = append(msgs, sms.Message{
			AndroidID: int64(i), Address: fmt.Sprintf("+1555000000%d", i),
			Body: "hi", Date: int64(i) * 100, Type: sms.TypeReceived,
		})
	}
	if _, err := db.ImportMessages("dev1", msgs, true); err != nil {
		t.Fatal(err)
	}

	first, err := db.Conversations(0, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := db.Conversations(0, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(next) != 2 {
		t.Fatalf("page sizes: %d and %d, want 2 and 2", len(first), len(next))
	}
	// Newest first, and the pages must not overlap.
	if first[0].Address != "+15550000005" || first[1].Address != "+15550000004" {
		t.Errorf("first page = %v, %v", first[0].Address, first[1].Address)
	}
	if next[0].Address != "+15550000003" || next[1].Address != "+15550000002" {
		t.Errorf("second page = %v, %v", next[0].Address, next[1].Address)
	}
}

// The contact name is resolved in Go now that the SQL join is gone; it must
// still match across differing phone-number formatting.
func TestConversationsResolveContactName(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertContacts("dev1", []Contact{{Phone: "555-123-4567", Name: "Ada"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportMessages("dev1", []sms.Message{
		{AndroidID: 1, Address: "+1 (555) 123-4567", Body: "hi", Date: 100, Type: sms.TypeReceived},
	}, true); err != nil {
		t.Fatal(err)
	}
	convs, err := db.Conversations(0, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || convs[0].ContactName != "Ada" {
		t.Fatalf("got %+v, want contact name Ada", convs)
	}
}

func TestMessagesPaging(t *testing.T) {
	db := openTestDB(t)
	var msgs []sms.Message
	for i := 1; i <= 10; i++ {
		msgs = append(msgs, msg(int64(i), sms.TypeReceived, fmt.Sprintf("m%d", i)))
	}
	if _, err := db.ImportMessages("dev1", msgs, true); err != nil {
		t.Fatal(err)
	}
	const addr = "+15551234567"

	page, err := db.Messages(addr, 0, Cursor{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore {
		t.Error("HasMore = false, want true with 10 messages and a limit of 4")
	}
	// The newest window, returned oldest-first for rendering.
	if got := bodies(page.Messages); got != "m7 m8 m9 m10" {
		t.Errorf("first page = %q, want \"m7 m8 m9 m10\"", got)
	}

	oldest := page.Messages[0]
	older, err := db.Messages(addr, 0, Cursor{Date: oldest.Date, AndroidID: oldest.AndroidID}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(older.Messages); got != "m3 m4 m5 m6" {
		t.Errorf("second page = %q, want \"m3 m4 m5 m6\"", got)
	}

	oldest = older.Messages[0]
	last, err := db.Messages(addr, 0, Cursor{Date: oldest.Date, AndroidID: oldest.AndroidID}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if last.HasMore {
		t.Error("HasMore = true on the final page")
	}
	if got := bodies(last.Messages); got != "m1 m2" {
		t.Errorf("final page = %q, want \"m1 m2\"", got)
	}
}

// Messages sharing a timestamp straddle a page boundary; the android_id
// tie-break is what stops one from being skipped or repeated.
func TestMessagesPagingSameTimestamp(t *testing.T) {
	db := openTestDB(t)
	var msgs []sms.Message
	for i := 1; i <= 4; i++ {
		m := msg(int64(i), sms.TypeReceived, fmt.Sprintf("m%d", i))
		m.Date = 500 // identical for every message
		msgs = append(msgs, m)
	}
	if _, err := db.ImportMessages("dev1", msgs, true); err != nil {
		t.Fatal(err)
	}
	const addr = "+15551234567"

	page, err := db.Messages(addr, 0, Cursor{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(page.Messages); got != "m3 m4" {
		t.Fatalf("first page = %q, want \"m3 m4\"", got)
	}
	oldest := page.Messages[0]
	older, err := db.Messages(addr, 0, Cursor{Date: oldest.Date, AndroidID: oldest.AndroidID}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(older.Messages); got != "m1 m2" {
		t.Errorf("second page = %q, want \"m1 m2\"", got)
	}
}

// The time window is the viewer's main lever: a conversation with no traffic
// inside it must disappear entirely, and counts must describe the window.
func TestConversationsSinceWindow(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportMessages("dev1", []sms.Message{
		{AndroidID: 1, Address: "+15550000001", Body: "ancient", Date: 1_000, Type: sms.TypeReceived},
		{AndroidID: 2, Address: "+15550000001", Body: "recent", Date: 9_000, Type: sms.TypeReceived},
		{AndroidID: 3, Address: "+15550000002", Body: "only old", Date: 2_000, Type: sms.TypeReceived},
	}, true); err != nil {
		t.Fatal(err)
	}

	all, err := db.Conversations(0, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unwindowed returned %d conversations, want 2", len(all))
	}

	win, err := db.Conversations(5_000, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(win) != 1 {
		t.Fatalf("windowed returned %d conversations, want 1", len(win))
	}
	if win[0].Address != "+15550000001" || win[0].LastBody != "recent" {
		t.Errorf("conv = %+v, want the conversation with in-window traffic", win[0])
	}
	// Count describes the window, not all time: one of its two messages.
	if win[0].Count != 1 {
		t.Errorf("Count = %d, want 1 (in-window messages only)", win[0].Count)
	}
}

// A thread must honour the same window, and paging inside it must not leak
// messages from before the floor.
func TestMessagesSinceWindow(t *testing.T) {
	db := openTestDB(t)
	var msgs []sms.Message
	for i := 1; i <= 10; i++ {
		m := msg(int64(i), sms.TypeReceived, fmt.Sprintf("m%d", i))
		m.Date = int64(i) * 100
		msgs = append(msgs, m)
	}
	if _, err := db.ImportMessages("dev1", msgs, true); err != nil {
		t.Fatal(err)
	}
	const addr = "+15551234567"

	// Floor at 800 keeps m8, m9, m10.
	page, err := db.Messages(addr, 800, Cursor{}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(page.Messages); got != "m8 m9 m10" {
		t.Errorf("windowed page = %q, want \"m8 m9 m10\"", got)
	}
	if page.HasMore {
		t.Error("HasMore = true, want false — nothing older remains inside the window")
	}

	// Paging within the window stops at the floor rather than crossing it.
	first, err := db.Messages(addr, 800, Cursor{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(first.Messages); got != "m9 m10" || !first.HasMore {
		t.Fatalf("first windowed page = %q has_more=%v", got, first.HasMore)
	}
	oldest := first.Messages[0]
	next, err := db.Messages(addr, 800, Cursor{Date: oldest.Date, AndroidID: oldest.AndroidID}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(next.Messages); got != "m8" {
		t.Errorf("second windowed page = %q, want \"m8\" and no crossing of the floor", got)
	}

	// Dropping the floor from the same cursor is how the viewer widens a thread.
	wider, err := db.Messages(addr, 0, Cursor{Date: oldest.Date, AndroidID: oldest.AndroidID}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(wider.Messages); got != "m6 m7 m8" {
		t.Errorf("widened page = %q, want \"m6 m7 m8\"", got)
	}
}

func TestState(t *testing.T) {
	db := openTestDB(t)
	empty, err := db.State()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Count != 0 || empty.MaxID != 0 {
		t.Fatalf("empty state = %+v, want zeroes", empty)
	}

	if _, err := db.ImportMessages("dev1", []sms.Message{
		msg(1, sms.TypeReceived, "hi"),
		msg(2, sms.TypeSent, "bye"),
	}, true); err != nil {
		t.Fatal(err)
	}
	after, err := db.State()
	if err != nil {
		t.Fatal(err)
	}
	if after.Count != 2 || after.MaxID == 0 {
		t.Errorf("state = %+v, want count 2 and a non-zero max id", after)
	}
}

func bodies(msgs []StoredMessage) string {
	var parts []string
	for _, m := range msgs {
		parts = append(parts, m.Body)
	}
	return strings.Join(parts, " ")
}

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"+1 (555) 123-4567": "5551234567",
		"555-123-4567":      "5551234567",
		"5551234567":        "5551234567",
		"12345":             "12345",
		"":                  "",
		"NoDigitsHere":      "",
	}
	for in, want := range cases {
		if got := NormalizePhone(in); got != want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}
