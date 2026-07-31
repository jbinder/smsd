// Package database owns the local SQLite store: schema, deduplicated SMS
// import, the contact cache and the read-only queries the viewer uses. It uses
// the pure-Go modernc.org/sqlite driver so the binary needs no cgo.
package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/jbinder/smsd/internal/sms"
)

// DB wraps the SQLite connection and the smsd schema.
type DB struct {
	db *sql.DB
}

// Contact is a cached entry from the Android Contacts Provider.
type Contact struct {
	Phone string
	Name  string
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema migrations.
func Open(path string) (*DB, error) {
	// Pragmas via DSN: WAL for concurrent reads by the UI while importing,
	// busy_timeout so brief writer locks retry instead of erroring.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}
	// A single writer avoids SQLITE_BUSY churn; reads still work concurrently
	// under WAL. Keeping the pool tiny also keeps idle memory low.
	sqldb.SetMaxOpenConns(1)
	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("pinging db: %w", err)
	}
	d := &DB{db: sqldb}
	if err := d.migrate(); err != nil {
		sqldb.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the underlying database.
func (d *DB) Close() error { return d.db.Close() }

func (d *DB) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS messages (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	device        TEXT    NOT NULL,
	android_id    INTEGER NOT NULL,
	thread_id     INTEGER NOT NULL DEFAULT 0,
	address       TEXT    NOT NULL DEFAULT '',
	body          TEXT    NOT NULL DEFAULT '',
	date          INTEGER NOT NULL DEFAULT 0,
	date_sent     INTEGER NOT NULL DEFAULT 0,
	type          INTEGER NOT NULL DEFAULT 0,
	read          INTEGER NOT NULL DEFAULT 0,
	notified      INTEGER NOT NULL DEFAULT 0,
	imported_at   INTEGER NOT NULL DEFAULT 0,
	UNIQUE(device, android_id)
);
-- (address, date) serves both the per-address grouping behind the conversation
-- list and the keyset paging in Messages. It covers everything the older
-- address-only index did, which is dropped below so imports do not maintain two.
CREATE INDEX IF NOT EXISTS idx_messages_addr_date ON messages(address, date);
DROP INDEX IF EXISTS idx_messages_address;
CREATE INDEX IF NOT EXISTS idx_messages_thread  ON messages(thread_id);
CREATE INDEX IF NOT EXISTS idx_messages_date    ON messages(date);

CREATE TABLE IF NOT EXISTS contacts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	device      TEXT NOT NULL,
	phone       TEXT NOT NULL,
	normalized  TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	UNIQUE(device, normalized)
);
CREATE INDEX IF NOT EXISTS idx_contacts_norm ON contacts(normalized);
`
	_, err := d.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("migrating schema: %w", err)
	}
	return nil
}

// MaxAndroidID returns the highest device _id already imported for a device, or
// (0, false) if none exist yet. The caller uses this both to query only newer
// rows and to decide whether this is a first-time backfill.
func (d *DB) MaxAndroidID(device string) (int64, bool, error) {
	var max sql.NullInt64
	err := d.db.QueryRow(
		`SELECT MAX(android_id) FROM messages WHERE device = ?`, device,
	).Scan(&max)
	if err != nil {
		return 0, false, err
	}
	if !max.Valid {
		return 0, false, nil
	}
	return max.Int64, true, nil
}

// ImportResult reports the outcome of an import batch.
type ImportResult struct {
	Inserted int
	// NewlyReceived are received messages inserted in this batch that have not
	// been notified yet (empty during a first-time backfill).
	NewlyReceived []sms.Message
}

// ImportMessages inserts a batch of messages for a device, skipping any that
// already exist (dedup on device + android_id). When backfill is true the batch
// is treated as historical: rows are marked already-notified so the user is not
// flooded with notifications the first time a phone is connected.
//
// Only received messages inserted in a non-backfill batch are returned in
// NewlyReceived for the caller to notify on.
func (d *DB) ImportMessages(device string, msgs []sms.Message, backfill bool) (ImportResult, error) {
	var res ImportResult
	if len(msgs) == 0 {
		return res, nil
	}

	tx, err := d.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO messages
			(device, android_id, thread_id, address, body, date, date_sent, type, read, notified, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return res, err
	}
	defer stmt.Close()

	now := time.Now().UnixMilli()
	for _, m := range msgs {
		// Historical rows are pre-marked notified; live received rows are not,
		// so a later pass can pick them up for notification.
		notified := 1
		if !backfill && m.Received() {
			notified = 0
		}
		read := 0
		if m.Read {
			read = 1
		}
		r, err := stmt.Exec(device, m.AndroidID, m.ThreadID, m.Address, m.Body,
			m.Date, m.DateSent, m.Type, read, notified, now)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Inserted++
			if !backfill && m.Received() {
				res.NewlyReceived = append(res.NewlyReceived, m)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// UnreadCount returns the number of imported received messages not yet marked
// as notified (the tray's "unread" indicator).
func (d *DB) UnreadCount() (int, error) {
	var n int
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE notified = 0 AND type = ?`, sms.TypeReceived,
	).Scan(&n)
	return n, err
}

// MarkAllNotified clears the pending-notification flag on all messages. Used by
// the "Mark notifications read" tray action.
func (d *DB) MarkAllNotified() error {
	_, err := d.db.Exec(`UPDATE messages SET notified = 1 WHERE notified = 0`)
	return err
}

// UpsertContacts refreshes the contact cache for a device, inserting new
// numbers and updating names for existing ones.
func (d *DB) UpsertContacts(device string, contacts []Contact) error {
	if len(contacts) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO contacts (device, phone, normalized, name)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(device, normalized) DO UPDATE SET name = excluded.name, phone = excluded.phone`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range contacts {
		norm := NormalizePhone(c.Phone)
		if norm == "" {
			continue
		}
		if _, err := stmt.Exec(device, c.Phone, norm, c.Name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NormalizePhone reduces a phone number to comparable digits, keeping only the
// last 10 significant digits so that "+1 (555) 123-4567" and "555-123-4567"
// resolve to the same contact.
func NormalizePhone(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if len(digits) > 10 {
		digits = digits[len(digits)-10:]
	}
	return digits
}

// ContactName returns the cached contact name for an address, or "" if unknown.
func (d *DB) ContactName(address string) (string, error) {
	norm := NormalizePhone(address)
	if norm == "" {
		return "", nil
	}
	var name string
	err := d.db.QueryRow(
		`SELECT name FROM contacts WHERE normalized = ? AND name <> '' LIMIT 1`, norm,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// Conversation summarises a thread of messages with one counterparty.
type Conversation struct {
	Address     string `json:"address"`
	ContactName string `json:"contact_name"`
	LastBody    string `json:"last_body"`
	LastDate    int64  `json:"last_date"`
	Count       int    `json:"count"`
}

// Conversations returns one row per address, most recently active first, with
// the contact name resolved from the cache. It returns at most limit rows
// starting at offset so the viewer can render the list incrementally.
//
// since is a millisecond epoch floor: only messages at or after it are
// considered, so a conversation with no traffic in the window does not appear
// at all and Count reflects the window rather than all time. Zero means no
// floor. This is the viewer's main lever — a month's window over a six-year
// history is a handful of conversations instead of hundreds.
//
// The contact name is resolved in Go rather than by joining contacts: the join
// predicate has to normalise the address, and no index can serve that
// expression, so it degrades into a scan of the whole contact table per row.
func (d *DB) Conversations(since int64, limit, offset int) ([]Conversation, error) {
	names, err := d.contactNames()
	if err != nil {
		return nil, err
	}

	// Group first, then fetch each preview body in a correlated lookup. Ranking
	// every message with a window function instead costs an order of magnitude
	// more (64ms vs 6ms over 17k messages) because it sorts the whole table;
	// here the grouping is a covering-index scan and only the rows actually on
	// the page pay for a body lookup. The android_id tie-break keeps the
	// preview deterministic when two messages share a millisecond.
	rows, err := d.db.Query(`
		SELECT g.address, g.n, g.d,
		       (SELECT body FROM messages m
		         WHERE m.address = g.address AND m.date = g.d
		         ORDER BY m.android_id DESC LIMIT 1)
		FROM (
			SELECT address, COUNT(*) AS n, MAX(date) AS d
			FROM messages WHERE date >= ? GROUP BY address
			ORDER BY d DESC, address
			LIMIT ? OFFSET ?
		) g`, since, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.Address, &c.Count, &c.LastDate, &c.LastBody); err != nil {
			return nil, err
		}
		c.ContactName = names[NormalizePhone(c.Address)]
		out = append(out, c)
	}
	return out, rows.Err()
}

// contactNames maps a normalised phone number to its cached contact name.
func (d *DB) contactNames() (map[string]string, error) {
	rows, err := d.db.Query(`SELECT normalized, name FROM contacts WHERE name <> ''`)
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

// StoredMessage is a message as returned to the viewer.
type StoredMessage struct {
	AndroidID int64  `json:"android_id"`
	Address   string `json:"address"`
	Body      string `json:"body"`
	Date      int64  `json:"date"`
	Type      int    `json:"type"`
	Received  bool   `json:"received"`
}

// Cursor identifies the oldest message a caller already holds. The zero Cursor
// starts at the newest message in the thread.
type Cursor struct {
	Date      int64
	AndroidID int64
}

// MessagePage is one window of a conversation.
type MessagePage struct {
	// Messages are chronological, oldest first, ready to render top to bottom.
	Messages []StoredMessage `json:"messages"`
	// HasMore reports whether older messages exist before this page.
	HasMore bool `json:"has_more"`
}

// Messages returns up to limit messages for an address, ending at cur and
// walking backwards in time. since bounds the window the same way it does in
// Conversations; zero means no floor. Threads here run to thousands of
// messages, so even inside a window the viewer pages rather than loading a
// whole thread at once.
func (d *DB) Messages(address string, since int64, cur Cursor, limit int) (MessagePage, error) {
	// Selecting one extra row is how we learn whether older messages remain
	// without paying for a second COUNT query.
	args := []any{address, since}
	where := ""
	if cur.Date > 0 || cur.AndroidID > 0 {
		// (date, android_id) is the sort key, so the cursor has to compare on
		// both: date alone is not unique and would drop or repeat the messages
		// that straddle a page boundary.
		where = ` AND (date < ? OR (date = ? AND android_id < ?))`
		args = append(args, cur.Date, cur.Date, cur.AndroidID)
	}
	args = append(args, limit+1)

	rows, err := d.db.Query(`
		SELECT android_id, address, body, date, type
		FROM messages WHERE address = ? AND date >= ?`+where+`
		ORDER BY date DESC, android_id DESC
		LIMIT ?`, args...)
	if err != nil {
		return MessagePage{}, err
	}
	defer rows.Close()

	msgs, err := scanMessages(rows)
	if err != nil {
		return MessagePage{}, err
	}

	page := MessagePage{}
	if len(msgs) > limit {
		page.HasMore = true
		msgs = msgs[:limit]
	}
	// The query walks backwards; the viewer renders forwards.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	page.Messages = msgs
	return page, nil
}

// State is a cheap fingerprint of the message table. The viewer polls it and
// only re-renders when it changes, which keeps an idle window quiet.
type State struct {
	Count int64 `json:"count"`
	MaxID int64 `json:"max_id"`
}

// State returns the current message count and highest row id.
func (d *DB) State() (State, error) {
	var s State
	err := d.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM messages`).Scan(&s.Count, &s.MaxID)
	return s, err
}

// Search finds messages whose body, address or resolved contact name matches
// the query (case-insensitive substring), newest first.
func (d *DB) Search(query string) ([]StoredMessage, error) {
	q := "%" + strings.ToLower(query) + "%"
	norm := NormalizePhone(query)
	normLike := "%" + norm + "%"
	rows, err := d.db.Query(`
		SELECT DISTINCT m.android_id, m.address, m.body, m.date, m.type
		FROM messages m
		LEFT JOIN contacts c ON c.normalized = `+normalizeSQL("m.address")+`
		WHERE lower(m.body) LIKE ?
		   OR lower(m.address) LIKE ?
		   OR (? <> '' AND m.address LIKE ?)
		   OR lower(c.name) LIKE ?
		ORDER BY m.date DESC
		LIMIT 500`, q, q, norm, normLike, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]StoredMessage, error) {
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.AndroidID, &m.Address, &m.Body, &m.Date, &m.Type); err != nil {
			return nil, err
		}
		m.Received = m.Type == sms.TypeReceived
		out = append(out, m)
	}
	return out, rows.Err()
}

// normalizeSQL returns a SQLite expression that normalises a phone column the
// same way NormalizePhone does (digits only, last 10). Kept in one place so the
// Go and SQL normalisation stay consistent.
func normalizeSQL(col string) string {
	// Strip common separators then take the rightmost 10 characters.
	expr := col
	for _, ch := range []string{"'+'", "'-'", "' '", "'('", "')'", "'.'"} {
		expr = "REPLACE(" + expr + ", " + ch + ", '')"
	}
	return "substr(" + expr + ", -10)"
}
