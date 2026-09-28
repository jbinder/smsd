package database

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/jbinder/smsd/internal/contact"
	"github.com/jbinder/smsd/internal/sms"
)

func phone(id, cid int64, value string) contact.Detail {
	return contact.Detail{AndroidID: id, ContactID: cid, Kind: contact.KindPhone, Label: "Mobile", Value: value}
}

// cardByName finds a contact by name through the viewer's API.
func cardByName(t *testing.T, db *DB, name string) ContactCard {
	t.Helper()
	list, err := db.Contacts()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Name == name {
			card, ok, err := db.Contact(c.ID)
			if err != nil || !ok {
				t.Fatalf("Contact(%d) = %v, %v", c.ID, ok, err)
			}
			return card
		}
	}
	t.Fatalf("no contact named %q in %+v", name, list)
	return ContactCard{}
}

func TestSyncContactsMarksDeletedAndRestores(t *testing.T) {
	db := openTestDB(t)
	ada := contact.Contact{AndroidID: 1, Name: "Ada"}
	bob := contact.Contact{AndroidID: 2, Name: "Bob"}

	res, err := db.SyncContacts("dev1", []contact.Contact{ada, bob},
		[]contact.Detail{phone(10, 1, "+43 660 1111111"), phone(11, 1, "+43 660 2222222"), phone(20, 2, "555-0100")})
	if err != nil {
		t.Fatal(err)
	}
	if res != (SyncResult{Added: 2}) {
		t.Errorf("first sync = %+v, want 2 added", res)
	}

	// Bob and one of Ada's numbers disappear from the phone.
	res, err = db.SyncContacts("dev1", []contact.Contact{ada}, []contact.Detail{phone(10, 1, "+43 660 1111111")})
	if err != nil {
		t.Fatal(err)
	}
	if res != (SyncResult{Removed: 1}) {
		t.Errorf("second sync = %+v, want 1 removed", res)
	}

	// Nothing is removed from the store, only marked.
	if got := cardByName(t, db, "Bob"); got.DeletedAt == 0 || len(got.Details) != 1 || got.Details[0].DeletedAt == 0 {
		t.Errorf("Bob = %+v, want marked deleted with his number kept", got)
	}
	card := cardByName(t, db, "Ada")
	if card.DeletedAt != 0 || len(card.Details) != 2 {
		t.Fatalf("Ada = %+v, want live with both numbers", card)
	}
	if card.Details[0].DeletedAt != 0 || card.Details[1].DeletedAt == 0 {
		t.Errorf("Ada's numbers = %+v, want the live one first and the removed one marked", card.Details)
	}
	// A deleted contact still names its old conversation.
	if name, _ := db.ContactName("5550100"); name != "Bob" {
		t.Errorf("ContactName for deleted contact = %q, want Bob", name)
	}

	// Bob comes back, renamed.
	bob.Name = "Robert"
	res, err = db.SyncContacts("dev1", []contact.Contact{ada, bob},
		[]contact.Detail{phone(10, 1, "+43 660 1111111"), phone(20, 2, "555-0100")})
	if err != nil {
		t.Fatal(err)
	}
	if res != (SyncResult{Restored: 1}) {
		t.Errorf("third sync = %+v, want 1 restored", res)
	}
	if got := cardByName(t, db, "Robert"); got.DeletedAt != 0 || got.Details[0].DeletedAt != 0 {
		t.Errorf("Robert = %+v, want live again", got)
	}
}

// A number held by a deleted contact and a live one resolves to the live one.
func TestContactNamePrefersLiveContact(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SyncContacts("dev1",
		[]contact.Contact{{AndroidID: 1, Name: "Old"}},
		[]contact.Detail{phone(10, 1, "555-0100")}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SyncContacts("dev1",
		[]contact.Contact{{AndroidID: 2, Name: "New"}},
		[]contact.Detail{phone(20, 2, "(555) 0100")}); err != nil {
		t.Fatal(err)
	}
	if name, _ := db.ContactName("5550100"); name != "New" {
		t.Errorf("ContactName = %q, want New", name)
	}
	if _, err := db.ImportMessages("dev1", []sms.Message{
		{AndroidID: 1, Address: "5550100", Body: "hi", Date: 100, Type: sms.TypeReceived},
	}, true); err != nil {
		t.Fatal(err)
	}
	convs, err := db.Conversations(0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || convs[0].ContactName != "New" {
		t.Errorf("conversations = %+v, want name New", convs)
	}
}

func TestContactCardDedupesAndLinksConversation(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportMessages("dev1", []sms.Message{
		{AndroidID: 1, Address: "+436601111111", Body: "hi", Date: 100, Type: sms.TypeReceived},
	}, true); err != nil {
		t.Fatal(err)
	}
	// Two accounts on the phone carry the same number, formatted differently.
	if _, err := db.SyncContacts("dev1",
		[]contact.Contact{{AndroidID: 1, Name: "Ada", Starred: true}},
		[]contact.Detail{
			{AndroidID: 12, ContactID: 1, Kind: contact.KindNote, Value: "met at, the conference\nsecond line"},
			phone(10, 1, "+43 660 1111111"),
			phone(11, 1, "0043660 1111111"),
			{AndroidID: 13, ContactID: 1, Kind: contact.KindEmail, Label: "Work", Value: "ada@example.org"},
		}); err != nil {
		t.Fatal(err)
	}
	card := cardByName(t, db, "Ada")
	if !card.Starred || card.Phone != "+43 660 1111111" {
		t.Errorf("card = %+v", card.ContactSummary)
	}
	kinds := ""
	for _, d := range card.Details {
		kinds += d.Kind + " "
	}
	if kinds != "phone email note " {
		t.Fatalf("detail kinds = %q, want phone, email, note once each", kinds)
	}
	if card.Details[0].Conversation != "+436601111111" {
		t.Errorf("phone conversation = %q, want +436601111111", card.Details[0].Conversation)
	}
	if card.Details[2].Value != "met at, the conference\nsecond line" {
		t.Errorf("note = %q", card.Details[2].Value)
	}
	if _, ok, err := db.Contact(9999); ok || err != nil {
		t.Errorf("Contact(9999) = %v, %v; want not found", ok, err)
	}
}

// Databases from before contacts were synced hold a number-to-name cache. The
// first sync keeps numbers still on the phone as synced and turns the rest
// into deleted contacts, rather than losing them.
func TestLegacyContactCacheIsFolded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE contacts (
			id INTEGER PRIMARY KEY AUTOINCREMENT, device TEXT NOT NULL, phone TEXT NOT NULL,
			normalized TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', UNIQUE(device, normalized));
		CREATE INDEX idx_contacts_norm ON contacts(normalized);
		INSERT INTO contacts (device, phone, normalized, name) VALUES
			('dev1', '555-0100', '5550100', 'Ada'),
			('dev1', '555-0199', '5550199', 'Gone'),
			('dev1', '555-0198', '5550198', 'Gone');`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Before the first sync the old names are not consulted, but not lost.
	if _, err := db.SyncContacts("dev1",
		[]contact.Contact{{AndroidID: 1, Name: "Ada"}},
		[]contact.Detail{phone(10, 1, "555-0100")}); err != nil {
		t.Fatal(err)
	}
	list, err := db.Contacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("contacts = %+v, want Ada and one deleted Gone", list)
	}
	gone := cardByName(t, db, "Gone")
	if gone.DeletedAt == 0 || len(gone.Details) != 2 {
		t.Errorf("Gone = %+v, want deleted with both numbers", gone)
	}
	if name, _ := db.ContactName("555-0199"); name != "Gone" {
		t.Errorf("ContactName = %q, want Gone", name)
	}
	var tables int
	db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'contacts_legacy'`).Scan(&tables)
	if tables != 0 {
		t.Errorf("contacts_legacy still present after folding")
	}

	// A second sync must not duplicate or resurrect the folded contacts.
	if _, err := db.SyncContacts("dev1",
		[]contact.Contact{{AndroidID: 1, Name: "Ada"}},
		[]contact.Detail{phone(10, 1, "555-0100")}); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.Contacts(); len(list) != 2 {
		t.Errorf("after resync contacts = %+v, want 2", list)
	}
	if gone := cardByName(t, db, "Gone"); gone.DeletedAt == 0 {
		t.Errorf("folded contact was resurrected")
	}
}

func TestReconcileMessages(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportMessages("dev1", []sms.Message{
		msg(1, sms.TypeReceived, "a"), msg(2, sms.TypeSent, "b"), msg(3, sms.TypeReceived, "c"),
	}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportMessages("dev2", []sms.Message{msg(2, sms.TypeReceived, "other phone")}, true); err != nil {
		t.Fatal(err)
	}

	// Message 2 was deleted on dev1; 4 is on the phone but not imported yet.
	res, err := db.ReconcileMessages("dev1", []int64{1, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if res != (SyncResult{Removed: 1}) {
		t.Errorf("reconcile = %+v, want 1 removed", res)
	}
	page, err := db.Messages("+15551234567", 0, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 4 {
		t.Fatalf("got %d messages, want all 4 kept", len(page.Messages))
	}
	for _, m := range page.Messages {
		deleted := m.DeletedAt != 0
		if want := m.AndroidID == 2 && m.Body == "b"; deleted != want {
			t.Errorf("message %d %q deleted = %v, want %v", m.AndroidID, m.Body, deleted, want)
		}
	}
	if s, _ := db.State(); s.Deleted != 1 {
		t.Errorf("State.Deleted = %d, want 1", s.Deleted)
	}
	if hits, _ := db.Search("b"); len(hits) != 1 || hits[0].DeletedAt == 0 {
		t.Errorf("search = %+v, want the deleted message, marked", hits)
	}

	// It reappears (e.g. restored from the phone's trash).
	res, err = db.ReconcileMessages("dev1", []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if res != (SyncResult{Restored: 1}) {
		t.Errorf("reconcile = %+v, want 1 restored", res)
	}
	if s, _ := db.State(); s.Deleted != 0 {
		t.Errorf("State.Deleted = %d, want 0", s.Deleted)
	}
}

// Opening a database from before deletion tracking adds the column in place.
func TestMessagesDeletedAtMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT, device TEXT NOT NULL, android_id INTEGER NOT NULL,
			thread_id INTEGER NOT NULL DEFAULT 0, address TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL DEFAULT '', date INTEGER NOT NULL DEFAULT 0,
			date_sent INTEGER NOT NULL DEFAULT 0, type INTEGER NOT NULL DEFAULT 0,
			read INTEGER NOT NULL DEFAULT 0, notified INTEGER NOT NULL DEFAULT 0,
			imported_at INTEGER NOT NULL DEFAULT 0, UNIQUE(device, android_id));
		INSERT INTO messages (device, android_id, address, body, date, type)
			VALUES ('dev1', 1, '555', 'kept', 100, 1);`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	page, err := db.Messages("555", 0, Cursor{}, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].DeletedAt != 0 {
		t.Fatalf("messages = %+v, %v", page, err)
	}
}
