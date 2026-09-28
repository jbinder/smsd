package database

import (
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/jbinder/smsd/internal/contact"
)

// Contacts mirror the phone's Contacts Provider: one row per aggregate contact
// and one per detail (number, e-mail address, …), each keyed by its id on the
// device. Like messages, nothing is ever deleted: a contact or detail that
// disappears from the phone keeps its row and gets deleted_at, the time smsd
// noticed. Edits on the phone (a renamed contact, a corrected number) update
// the row in place.
const contactsSchema = `
CREATE TABLE IF NOT EXISTS contacts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	device      TEXT    NOT NULL,
	android_id  INTEGER NOT NULL,
	lookup_key  TEXT    NOT NULL DEFAULT '',
	name        TEXT    NOT NULL DEFAULT '',
	starred     INTEGER NOT NULL DEFAULT 0,
	imported_at INTEGER NOT NULL DEFAULT 0,
	deleted_at  INTEGER,
	UNIQUE(device, android_id)
);

CREATE TABLE IF NOT EXISTS contact_details (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	device      TEXT    NOT NULL,
	android_id  INTEGER NOT NULL,
	contact_id  INTEGER NOT NULL, -- contacts.android_id of the owning contact
	kind        TEXT    NOT NULL,
	label       TEXT    NOT NULL DEFAULT '',
	value       TEXT    NOT NULL DEFAULT '',
	normalized  TEXT    NOT NULL DEFAULT '', -- NormalizePhone(value) for phones
	imported_at INTEGER NOT NULL DEFAULT 0,
	deleted_at  INTEGER,
	UNIQUE(device, android_id)
);
CREATE INDEX IF NOT EXISTS idx_contact_details_contact ON contact_details(device, contact_id);
CREATE INDEX IF NOT EXISTS idx_contact_details_norm    ON contact_details(normalized);
`

// migrateContacts creates the contact tables. Earlier versions kept only a
// number-to-name cache in a table also called contacts; it is renamed to
// contacts_legacy and folded in by the first sync (see foldLegacyContacts).
func (d *DB) migrateContacts() error {
	legacy, err := d.hasColumn("contacts", "normalized")
	if err != nil {
		return err
	}
	if legacy {
		if _, err := d.db.Exec(`
			DROP INDEX IF EXISTS idx_contacts_norm;
			ALTER TABLE contacts RENAME TO contacts_legacy;`); err != nil {
			return err
		}
	}
	_, err = d.db.Exec(contactsSchema)
	return err
}

// SyncContacts brings device's stored contacts in line with a full read of
// the phone's: new ones are added, changed ones updated, and ones no longer on
// the phone marked deleted. The Result counts contacts, not details.
//
// The caller must pass a complete read. An empty one is indistinguishable from
// a failed query that printed nothing, so it would mark everything deleted;
// callers skip the sync instead.
func (d *DB) SyncContacts(device string, contacts []contact.Contact, details []contact.Detail) (SyncResult, error) {
	var res SyncResult
	tx, err := d.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()

	known, err := storedIDs(tx, "contacts", device)
	if err != nil {
		return res, err
	}
	seen := make(map[int64]bool, len(contacts))
	stmt, err := tx.Prepare(`
		INSERT INTO contacts (device, android_id, lookup_key, name, starred, imported_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(device, android_id) DO UPDATE SET
			lookup_key = excluded.lookup_key, name = excluded.name, starred = excluded.starred`)
	if err != nil {
		return res, err
	}
	defer stmt.Close()
	for _, c := range contacts {
		seen[c.AndroidID] = true
		if !known[c.AndroidID] {
			res.Added++
		}
		if _, err := stmt.Exec(device, c.AndroidID, c.LookupKey, c.Name, c.Starred, now); err != nil {
			return res, err
		}
	}
	if err := markDeleted(tx, "contacts", device, seen, now, &res); err != nil {
		return res, err
	}

	seen = make(map[int64]bool, len(details))
	dstmt, err := tx.Prepare(`
		INSERT INTO contact_details (device, android_id, contact_id, kind, label, value, normalized, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(device, android_id) DO UPDATE SET
			contact_id = excluded.contact_id, kind = excluded.kind, label = excluded.label,
			value = excluded.value, normalized = excluded.normalized`)
	if err != nil {
		return res, err
	}
	defer dstmt.Close()
	for _, dt := range details {
		seen[dt.AndroidID] = true
		norm := ""
		if dt.Kind == contact.KindPhone {
			norm = NormalizePhone(dt.Value)
		}
		if _, err := dstmt.Exec(device, dt.AndroidID, dt.ContactID, dt.Kind, dt.Label,
			dt.Value, norm, now); err != nil {
			return res, err
		}
	}
	// Details only feed the contact counts through their contact; tally them
	// separately so a removed phone number does not read as a removed contact.
	if err := markDeleted(tx, "contact_details", device, seen, now, &SyncResult{}); err != nil {
		return res, err
	}

	if err := foldLegacyContacts(tx, device, now); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// storedIDs returns the android ids stored for device in table.
func storedIDs(tx *sql.Tx, table, device string) (map[int64]bool, error) {
	rows, err := tx.Query(`SELECT android_id FROM `+table+` WHERE device = ?`, device)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// foldLegacyContacts moves device's rows from the old number-to-name cache
// into the contact tables. Numbers the phone still has are already covered by
// the sync that just ran; the rest belong to contacts deleted from the phone
// before smsd kept them, and become deleted contacts — one per name. Negative
// android ids keep them clear of the phone's own. The legacy table is dropped
// once every device has been folded.
func foldLegacyContacts(tx *sql.Tx, device string, now int64) error {
	var exists int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'contacts_legacy'`,
	).Scan(&exists); err != nil || exists == 0 {
		return err
	}

	rows, err := tx.Query(`
		SELECT id, phone, normalized, name FROM contacts_legacy
		WHERE device = ? AND normalized NOT IN (
			SELECT normalized FROM contact_details WHERE device = ? AND kind = 'phone')
		ORDER BY id`, device, device)
	if err != nil {
		return err
	}
	type legacyRow struct {
		id                      int64
		phone, normalized, name string
	}
	var legacy []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.id, &r.phone, &r.normalized, &r.name); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	owner := map[string]int64{} // name -> synthetic contact id
	for _, r := range legacy {
		cid, ok := owner[r.name]
		if !ok || r.name == "" {
			cid = -r.id
			owner[r.name] = cid
			if _, err := tx.Exec(`
				INSERT INTO contacts (device, android_id, name, imported_at, deleted_at)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				device, cid, r.name, now, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO contact_details
				(device, android_id, contact_id, kind, value, normalized, imported_at, deleted_at)
			VALUES (?, ?, ?, 'phone', ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			device, -r.id, cid, r.phone, r.normalized, now, now); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`DELETE FROM contacts_legacy WHERE device = ?`, device); err != nil {
		return err
	}
	var left int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM contacts_legacy`).Scan(&left); err != nil {
		return err
	}
	if left == 0 {
		_, err = tx.Exec(`DROP TABLE contacts_legacy`)
	}
	return err
}

// phoneNamesSQL lists every phone number with its contact's name.
const phoneNamesSQL = `
	SELECT d.normalized, c.name
	FROM contact_details d
	JOIN contacts c ON c.device = d.device AND c.android_id = d.contact_id
	WHERE d.kind = 'phone' AND d.normalized <> '' AND c.name <> ''`

// onPhone is true for a number whose contact and detail are both still on the
// phone.
const onPhone = `(c.deleted_at IS NULL AND d.deleted_at IS NULL)`

// ContactName returns the contact name for an address, or "" if unknown. A
// contact deleted from the phone still names its old conversations, but one
// still on the phone takes precedence.
func (d *DB) ContactName(address string) (string, error) {
	norm := NormalizePhone(address)
	if norm == "" {
		return "", nil
	}
	var name string
	err := d.db.QueryRow(
		phoneNamesSQL+` AND d.normalized = ? ORDER BY `+onPhone+` DESC, c.id LIMIT 1`, norm,
	).Scan(&norm, &name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// contactNames maps a normalised phone number to its contact name, with the
// same precedence as ContactName: rows still on the phone come last, so they
// overwrite deleted ones.
func (d *DB) contactNames() (map[string]string, error) {
	rows, err := d.db.Query(phoneNamesSQL + ` ORDER BY ` + onPhone + `, c.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	names := map[string]string{}
	for rows.Next() {
		var norm, name string
		if err := rows.Scan(&norm, &name); err != nil {
			return nil, err
		}
		names[norm] = name
	}
	return names, rows.Err()
}

// ContactSummary is a contact as listed in the viewer.
type ContactSummary struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Starred bool   `json:"starred"`
	// Phone is the contact's first number, to tell same-named contacts apart.
	Phone string `json:"phone"`
	// DeletedAt is when smsd noticed the contact had gone from the phone, in
	// epoch milliseconds; zero while it is still there.
	DeletedAt int64 `json:"deleted_at,omitempty"`
}

// Contacts lists every stored contact, including deleted ones, by name.
func (d *DB) Contacts() ([]ContactSummary, error) {
	rows, err := d.db.Query(`
		SELECT c.id, c.name, c.starred, COALESCE(c.deleted_at, 0),
		       COALESCE((SELECT value FROM contact_details d
		                  WHERE d.device = c.device AND d.contact_id = c.android_id
		                    AND d.kind = 'phone'
		                  ORDER BY d.deleted_at IS NOT NULL, d.id LIMIT 1), '')
		FROM contacts c
		ORDER BY c.name = '', c.name COLLATE NOCASE, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContactSummary{}
	for rows.Next() {
		var c ContactSummary
		if err := rows.Scan(&c.ID, &c.Name, &c.Starred, &c.DeletedAt, &c.Phone); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ContactDetail is one field on a contact card.
type ContactDetail struct {
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Value     string `json:"value"`
	DeletedAt int64  `json:"deleted_at,omitempty"`
	// Conversation is the stored address of the SMS conversation with this
	// number, if there is one, so the viewer can link to it.
	Conversation string `json:"conversation,omitempty"`
}

// ContactCard is a contact with all its details.
type ContactCard struct {
	ContactSummary
	Details []ContactDetail `json:"details"`
}

// kindOrder is the order details appear on a card.
var kindOrder = map[string]int{
	contact.KindPhone: 0, contact.KindEmail: 1, contact.KindAddress: 2,
	contact.KindOrganization: 3, contact.KindWebsite: 4, contact.KindEvent: 5,
	contact.KindNickname: 6, contact.KindNote: 7,
}

// Contact returns the card for the contact with the given row id; ok is false
// if there is none.
func (d *DB) Contact(id int64) (card ContactCard, ok bool, err error) {
	var device string
	var androidID int64
	err = d.db.QueryRow(`
		SELECT id, name, starred, COALESCE(deleted_at, 0), device, android_id
		FROM contacts WHERE id = ?`, id,
	).Scan(&card.ID, &card.Name, &card.Starred, &card.DeletedAt, &device, &androidID)
	if err == sql.ErrNoRows {
		return card, false, nil
	}
	if err != nil {
		return card, false, err
	}

	rows, err := d.db.Query(`
		SELECT kind, label, value, normalized, COALESCE(deleted_at, 0)
		FROM contact_details WHERE device = ? AND contact_id = ?
		ORDER BY deleted_at IS NOT NULL, id`, device, androidID)
	if err != nil {
		return card, false, err
	}
	defer rows.Close()

	// Several accounts on the phone often carry the same number or address for
	// one person; show each once, preferring a copy that is still on the phone.
	seen := map[string]bool{}
	var norms []string
	card.Details = []ContactDetail{}
	for rows.Next() {
		var dt ContactDetail
		var norm string
		if err := rows.Scan(&dt.Kind, &dt.Label, &dt.Value, &norm, &dt.DeletedAt); err != nil {
			return card, false, err
		}
		key := dt.Kind + "\x00" + strings.ToLower(dt.Value)
		if norm != "" {
			key = dt.Kind + "\x00" + norm
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		card.Details = append(card.Details, dt)
		norms = append(norms, norm)
	}
	if err := rows.Err(); err != nil {
		return card, false, err
	}

	for _, dt := range card.Details {
		if dt.Kind == contact.KindPhone {
			card.Phone = dt.Value
			break
		}
	}
	convs, err := d.conversationsByNumber()
	if err != nil {
		return card, false, err
	}
	for i := range card.Details {
		if norms[i] != "" {
			card.Details[i].Conversation = convs[norms[i]]
		}
	}
	sort.SliceStable(card.Details, func(i, j int) bool {
		return kindOrder[card.Details[i].Kind] < kindOrder[card.Details[j].Kind]
	})
	return card, true, nil
}

// conversationsByNumber maps a normalised phone number to the address its
// messages are stored under. Grouping is served by the address index, and
// there are only as many rows as conversations.
func (d *DB) conversationsByNumber() (map[string]string, error) {
	rows, err := d.db.Query(`SELECT address FROM messages GROUP BY address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, err
		}
		if norm := NormalizePhone(addr); norm != "" {
			out[norm] = addr
		}
	}
	return out, rows.Err()
}
