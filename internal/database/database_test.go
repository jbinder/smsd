package database

import (
	"path/filepath"
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
	convs, err := db.Conversations()
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
