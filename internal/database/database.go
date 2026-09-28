// Package database owns the local SQLite store: schema, deduplicated SMS
// import, contacts, deletion tracking and the read-only queries the viewer
// uses. It uses the pure-Go modernc.org/sqlite driver so the binary needs no
// cgo.
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
`
	if _, err := d.db.Exec(schema); err != nil {
		return fmt.Errorf("migrating schema: %w", err)
	}

	// Nothing is ever deleted from the store. A message that disappears from
	// the phone keeps its row and gets deleted_at, the time smsd noticed.
	hasDeleted, err := d.hasColumn("messages", "deleted_at")
	if err != nil {
		return fmt.Errorf("migrating schema: %w", err)
	}
	if !hasDeleted {
		if _, err := d.db.Exec(`ALTER TABLE messages ADD COLUMN deleted_at INTEGER`); err != nil {
			return fmt.Errorf("adding messages.deleted_at: %w", err)
		}
	}

	if err := d.migrateContacts(); err != nil {
		return fmt.Errorf("migrating contacts: %w", err)
	}
	return nil
}

// hasColumn reports whether table has a column called name.
func (d *DB) hasColumn(table, name string) (bool, error) {
	var n int
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, name,
	).Scan(&n)
	return n > 0, err
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

// StoredMessage is a message as returned to the viewer.
type StoredMessage struct {
	AndroidID int64  `json:"android_id"`
	Address   string `json:"address"`
	Body      string `json:"body"`
	Date      int64  `json:"date"`
	Type      int    `json:"type"`
	Received  bool   `json:"received"`
	// DeletedAt is when smsd noticed the message had gone from the phone, in
	// epoch milliseconds; zero while it is still there.
	DeletedAt int64 `json:"deleted_at,omitempty"`
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
		SELECT android_id, address, body, date, type, COALESCE(deleted_at, 0)
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
	Count   int64 `json:"count"`
	MaxID   int64 `json:"max_id"`
	Deleted int64 `json:"deleted"`
}

// State returns the current message count, highest row id and the number of
// messages marked deleted.
func (d *DB) State() (State, error) {
	var s State
	err := d.db.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(id), 0), COUNT(deleted_at) FROM messages`,
	).Scan(&s.Count, &s.MaxID, &s.Deleted)
	return s, err
}

// SyncResult counts what a reconciliation against the phone changed.
type SyncResult struct {
	Added    int // new rows
	Removed  int // rows newly marked deleted
	Restored int // rows marked deleted that are back on the phone
}

// String summarises the non-zero counts, e.g. " (2 new, 1 deleted on phone)".
func (r SyncResult) String() string {
	var parts []string
	if r.Added > 0 {
		parts = append(parts, fmt.Sprintf("%d new", r.Added))
	}
	if r.Removed > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted on phone", r.Removed))
	}
	if r.Restored > 0 {
		parts = append(parts, fmt.Sprintf("%d back on phone", r.Restored))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// ReconcileMessages compares the ids currently on the phone with those stored
// for device. Stored messages missing from the phone are marked deleted — never
// removed — and a marked message that reappears is unmarked. Ids not stored yet
// are left to the regular import.
func (d *DB) ReconcileMessages(device string, onPhone []int64) (SyncResult, error) {
	seen := make(map[int64]bool, len(onPhone))
	for _, id := range onPhone {
		seen[id] = true
	}
	tx, err := d.db.Begin()
	if err != nil {
		return SyncResult{}, err
	}
	defer tx.Rollback()

	var res SyncResult
	if err := markDeleted(tx, "messages", device, seen, time.Now().UnixMilli(), &res); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// markDeleted brings the deleted_at marks of device's rows in table in line
// with seen, the android ids currently on the phone.
func markDeleted(tx *sql.Tx, table, device string, seen map[int64]bool, now int64, res *SyncResult) error {
	rows, err := tx.Query(
		`SELECT android_id, deleted_at IS NOT NULL FROM `+table+` WHERE device = ?`, device)
	if err != nil {
		return err
	}
	var gone, back []int64
	for rows.Next() {
		var id int64
		var deleted bool
		if err := rows.Scan(&id, &deleted); err != nil {
			rows.Close()
			return err
		}
		switch {
		case !seen[id] && !deleted:
			gone = append(gone, id)
		case seen[id] && deleted:
			back = append(back, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range gone {
		if _, err := tx.Exec(`UPDATE `+table+` SET deleted_at = ? WHERE device = ? AND android_id = ?`,
			now, device, id); err != nil {
			return err
		}
	}
	for _, id := range back {
		if _, err := tx.Exec(`UPDATE `+table+` SET deleted_at = NULL WHERE device = ? AND android_id = ?`,
			device, id); err != nil {
			return err
		}
	}
	res.Removed += len(gone)
	res.Restored += len(back)
	return nil
}

// Search finds messages whose body, address or resolved contact name matches
// the query (case-insensitive substring), newest first.
func (d *DB) Search(query string) ([]StoredMessage, error) {
	q := "%" + strings.ToLower(query) + "%"
	norm := NormalizePhone(query)
	normLike := "%" + norm + "%"
	rows, err := d.db.Query(`
		SELECT m.android_id, m.address, m.body, m.date, m.type, COALESCE(m.deleted_at, 0)
		FROM messages m
		WHERE lower(m.body) LIKE ?
		   OR lower(m.address) LIKE ?
		   OR (? <> '' AND m.address LIKE ?)
		   OR `+normalizeSQL("m.address")+` IN (
				SELECT d.normalized FROM contact_details d
				JOIN contacts c ON c.device = d.device AND c.android_id = d.contact_id
				WHERE d.kind = 'phone' AND lower(c.name) LIKE ?)
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
		if err := rows.Scan(&m.AndroidID, &m.Address, &m.Body, &m.Date, &m.Type, &m.DeletedAt); err != nil {
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
