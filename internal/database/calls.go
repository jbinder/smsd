package database

import (
	"database/sql"
	"time"

	"github.com/jbinder/smsd/internal/call"
)

// Calls mirror the phone's call log the way messages mirror its SMS: one row
// per entry, deduplicated on its id on the device, never deleted — an entry
// cleared from the phone's log keeps its row and gets deleted_at. notified
// marks missed calls the user has not been alerted about yet.
const callsSchema = `
CREATE TABLE IF NOT EXISTS calls (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	device        TEXT    NOT NULL,
	android_id    INTEGER NOT NULL,
	number        TEXT    NOT NULL DEFAULT '',
	date          INTEGER NOT NULL DEFAULT 0,
	duration      INTEGER NOT NULL DEFAULT 0,
	type          INTEGER NOT NULL DEFAULT 0,
	presentation  INTEGER NOT NULL DEFAULT 0,
	notified      INTEGER NOT NULL DEFAULT 0,
	imported_at   INTEGER NOT NULL DEFAULT 0,
	deleted_at    INTEGER,
	UNIQUE(device, android_id)
);
CREATE INDEX IF NOT EXISTS idx_calls_date ON calls(date);
`

// MaxCallID returns the highest call log _id imported for a device, or
// (0, false) if none have been.
func (d *DB) MaxCallID(device string) (int64, bool, error) {
	var max sql.NullInt64
	err := d.db.QueryRow(`SELECT MAX(android_id) FROM calls WHERE device = ?`, device).Scan(&max)
	if err != nil || !max.Valid {
		return 0, false, err
	}
	return max.Int64, true, nil
}

// CallImportResult reports the outcome of a call import batch.
type CallImportResult struct {
	Inserted int
	// NewlyMissed are missed calls inserted in this batch that have not been
	// notified yet (empty during a first-time backfill).
	NewlyMissed []call.Call
}

// ImportCalls inserts a batch of calls for a device, skipping any already
// stored. As with ImportMessages, a backfill is historical: its missed calls
// are pre-marked notified so a first connection does not raise an alert for
// every missed call in the log.
func (d *DB) ImportCalls(device string, calls []call.Call, backfill bool) (CallImportResult, error) {
	var res CallImportResult
	if len(calls) == 0 {
		return res, nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO calls
			(device, android_id, number, date, duration, type, presentation, notified, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return res, err
	}
	defer stmt.Close()

	now := time.Now().UnixMilli()
	for _, c := range calls {
		alert := !backfill && c.Missed()
		notified := 1
		if alert {
			notified = 0
		}
		r, err := stmt.Exec(device, c.AndroidID, c.Number, c.Date, c.Duration, c.Type,
			c.Presentation, notified, now)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Inserted++
			if alert {
				res.NewlyMissed = append(res.NewlyMissed, c)
			}
		}
	}
	return res, tx.Commit()
}

// ReconcileCalls marks stored calls no longer in the phone's call log as
// deleted, and unmarks any that came back. See ReconcileMessages.
func (d *DB) ReconcileCalls(device string, onPhone []int64) (SyncResult, error) {
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
	if err := markDeleted(tx, "calls", device, seen, time.Now().UnixMilli(), &res); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// StoredCall is a call as returned to the viewer.
type StoredCall struct {
	AndroidID    int64  `json:"android_id"`
	Number       string `json:"number"`
	ContactName  string `json:"contact_name,omitempty"`
	Caller       string `json:"caller"` // contact name, number, or "Private number"
	Date         int64  `json:"date"`
	Duration     int64  `json:"duration"`
	Type         int    `json:"type"`
	Presentation int    `json:"presentation"`
	// Conversation is the address of the SMS conversation with this number,
	// if there is one.
	Conversation string `json:"conversation,omitempty"`
	DeletedAt    int64  `json:"deleted_at,omitempty"`
	// Unread marks a missed call not yet seen in the viewer or cleared from
	// the tray.
	Unread bool `json:"unread,omitempty"`
}

// CallPage is one page of the call log, newest first.
type CallPage struct {
	Calls   []StoredCall `json:"calls"`
	HasMore bool         `json:"has_more"`
}

// Calls returns up to limit calls before cur, newest first. since is a
// millisecond floor as in Conversations; zero means no floor. A non-empty
// number restricts the page to calls with that number, compared the way
// NormalizePhone does, so "+43 660 1489613" finds "+436601489613".
func (d *DB) Calls(number string, since int64, cur Cursor, limit int) (CallPage, error) {
	where := `date >= ?`
	args := []any{since}
	if number != "" {
		norm := NormalizePhone(number)
		if norm == "" {
			// A withheld number: match the empty number column itself.
			where += ` AND number = ''`
		} else {
			where += ` AND ` + normalizeSQL("number") + ` = ?`
			args = append(args, norm)
		}
	}
	if cur.Date > 0 || cur.AndroidID > 0 {
		where += ` AND (date < ? OR (date = ? AND android_id < ?))`
		args = append(args, cur.Date, cur.Date, cur.AndroidID)
	}
	args = append(args, limit+1)

	rows, err := d.db.Query(`
		SELECT android_id, number, date, duration, type, presentation, COALESCE(deleted_at, 0),
		       notified = 0
		FROM calls WHERE `+where+`
		ORDER BY date DESC, android_id DESC
		LIMIT ?`, args...)
	if err != nil {
		return CallPage{}, err
	}
	defer rows.Close()

	page := CallPage{Calls: []StoredCall{}}
	for rows.Next() {
		var c StoredCall
		if err := rows.Scan(&c.AndroidID, &c.Number, &c.Date, &c.Duration, &c.Type,
			&c.Presentation, &c.DeletedAt, &c.Unread); err != nil {
			return page, err
		}
		page.Calls = append(page.Calls, c)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Calls) > limit {
		page.HasMore = true
		page.Calls = page.Calls[:limit]
	}

	// Names and conversations are resolved in Go for the same reason as in
	// Conversations: a join on a normalised number cannot use an index.
	names, err := d.contactNames()
	if err != nil {
		return page, err
	}
	convs, err := d.conversationsByNumber()
	if err != nil {
		return page, err
	}
	for i := range page.Calls {
		c := &page.Calls[i]
		norm := NormalizePhone(c.Number)
		c.ContactName = names[norm]
		c.Conversation = convs[norm]
		c.Caller = c.ContactName
		if c.Caller == "" {
			c.Caller = call.Call{Number: c.Number, Presentation: c.Presentation}.Caller()
		}
	}
	return page, nil
}
