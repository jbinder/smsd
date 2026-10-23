package database

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
)

// deviceTables are the tables whose rows belong to one phone, keyed by their
// device column.
var deviceTables = []string{"messages", "calls", "contacts", "contact_details", "contacts_legacy"}

// Devices lists the phones that have messages, calls or contacts stored,
// ordered by serial.
func (d *DB) Devices() ([]string, error) {
	rows, err := d.db.Query(`
		SELECT device FROM messages UNION SELECT device FROM calls
		UNION SELECT device FROM contacts ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ExportDevice writes a new database at path holding only device's rows. This
// database is left as it is. It fails if path already exists.
func (d *DB) ExportDevice(path, device string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	// VACUUM INTO copies schema, indexes and ids exactly; the other phones'
	// rows are then dropped from the copy only.
	if _, err := d.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("copying database: %w", err)
	}
	if err := keepOnly(path, device); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func keepOnly(path, device string) error {
	sqldb, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer sqldb.Close()
	sqldb.SetMaxOpenConns(1)

	tx, err := sqldb.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range deviceTables {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE device <> ?`, device); err != nil &&
			!strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("separating %s: %w", t, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = sqldb.Exec(`VACUUM`)
	return err
}
