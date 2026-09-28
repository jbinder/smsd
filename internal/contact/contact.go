// Package contact defines the contact data model and the parser for the output
// of `content query` against Android's Contacts Provider. Like package sms it
// is free of I/O so the parsing can be unit-tested in isolation.
package contact

import (
	"regexp"
	"strconv"
	"strings"
)

// ContactProjection is the column list requested from
// content://com.android.contacts/contacts. The _id is the aggregate contact id.
var ContactProjection = []string{"_id", "lookup", "starred", "display_name"}

// DetailProjection is the column list requested from
// content://com.android.contacts/data. data1 carries the value itself; data2 is
// the type code, data3 a custom label and data4 an organisation's job title.
var DetailProjection = []string{"_id", "contact_id", "mimetype", "data2", "data3", "data4", "data1"}

// Contact is one aggregate contact as Android's contacts app shows it.
type Contact struct {
	AndroidID int64  // contacts._id on the device
	LookupKey string // contacts.lookup, Android's more durable handle
	Name      string // display_name
	Starred   bool
}

// Detail kinds stored by smsd. Other data rows (photos, group memberships,
// messenger call actions, …) are skipped.
const (
	KindPhone        = "phone"
	KindEmail        = "email"
	KindAddress      = "address"
	KindOrganization = "organization"
	KindWebsite      = "website"
	KindEvent        = "event"
	KindNickname     = "nickname"
	KindNote         = "note"
)

var kindByMimetype = map[string]string{
	"vnd.android.cursor.item/phone_v2":          KindPhone,
	"vnd.android.cursor.item/email_v2":          KindEmail,
	"vnd.android.cursor.item/postal-address_v2": KindAddress,
	"vnd.android.cursor.item/organization":      KindOrganization,
	"vnd.android.cursor.item/website":           KindWebsite,
	"vnd.android.cursor.item/contact_event":     KindEvent,
	"vnd.android.cursor.item/nickname":          KindNickname,
	"vnd.android.cursor.item/note":              KindNote,
}

// Detail is one field of a contact: a phone number, an e-mail address, …
type Detail struct {
	AndroidID int64  // data._id on the device, stable for the life of the row
	ContactID int64  // aggregate contact it currently belongs to
	Kind      string // one of the Kind* constants
	Label     string // "Mobile", "Work", a custom label, or a job title
	Value     string
}

// typeLabels maps Android's per-kind data2 type codes to display labels. Code
// 0 (TYPE_CUSTOM) means the label is in data3 instead.
var typeLabels = map[string]map[int]string{
	KindPhone: {
		1: "Home", 2: "Mobile", 3: "Work", 4: "Work fax", 5: "Home fax", 6: "Pager",
		7: "Other", 8: "Callback", 9: "Car", 10: "Company main", 11: "ISDN", 12: "Main",
		13: "Other fax", 14: "Radio", 15: "Telex", 16: "TTY/TDD", 17: "Work mobile",
		18: "Work pager", 19: "Assistant", 20: "MMS",
	},
	KindEmail:   {1: "Home", 2: "Work", 3: "Other", 4: "Mobile"},
	KindAddress: {1: "Home", 2: "Work", 3: "Other"},
	KindEvent:   {1: "Anniversary", 2: "Other", 3: "Birthday"},
	KindWebsite: {1: "Homepage", 2: "Blog", 3: "Profile", 4: "Home", 5: "Work", 6: "FTP", 7: "Other"},
}

// rowSplit matches the "Row: <n> " prefix content query puts before each record.
var rowSplit = regexp.MustCompile(`(?m)^Row: \d+ `)

// ParseContacts parses the output of a contacts query made with
// ContactProjection. Rows without a usable _id are skipped.
func ParseContacts(raw string) []Contact {
	var out []Contact
	for _, f := range parseRows(raw, ContactProjection) {
		id, ok := parseID(f["_id"])
		if !ok {
			continue
		}
		out = append(out, Contact{
			AndroidID: id,
			LookupKey: f["lookup"],
			Name:      f["display_name"],
			Starred:   f["starred"] == "1",
		})
	}
	return out
}

// ParseDetails parses the output of a data query made with DetailProjection,
// keeping only the kinds smsd stores and rows that carry a value.
func ParseDetails(raw string) []Detail {
	var out []Detail
	for _, f := range parseRows(raw, DetailProjection) {
		kind := kindByMimetype[f["mimetype"]]
		if kind == "" {
			continue
		}
		id, ok := parseID(f["_id"])
		if !ok {
			continue
		}
		cid, ok := parseID(f["contact_id"])
		if !ok {
			continue
		}
		value := strings.TrimSpace(f["data1"])
		if value == "" {
			continue
		}
		out = append(out, Detail{
			AndroidID: id,
			ContactID: cid,
			Kind:      kind,
			Label:     label(kind, f),
			Value:     value,
		})
	}
	return out
}

func label(kind string, f map[string]string) string {
	if kind == KindOrganization {
		return strings.TrimSpace(f["data4"]) // job title
	}
	code, err := strconv.Atoi(f["data2"])
	if err != nil {
		return ""
	}
	if code == 0 {
		return strings.TrimSpace(f["data3"])
	}
	return typeLabels[kind][code]
}

// parseRows splits content query output into one column map per record, with
// NULL mapped to "".
//
// Unlike the SMS table, contact data has several free-text columns — notes and
// postal addresses span lines and contain ", " — so the trick of projecting the
// one free-text column last is not enough. Instead each value runs up to the
// next expected ", <column>=" marker, using the known projection order. A value
// would have to contain that exact marker to be misread.
func parseRows(raw string, cols []string) []map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "No result found") {
		return nil
	}
	var rows []map[string]string
	for _, chunk := range rowSplit.Split(raw, -1) {
		chunk = strings.TrimRight(chunk, "\r\n")
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		if f, ok := parseRow(chunk, cols); ok {
			rows = append(rows, f)
		}
	}
	return rows
}

func parseRow(chunk string, cols []string) (map[string]string, bool) {
	prefix := cols[0] + "="
	if !strings.HasPrefix(chunk, prefix) {
		return nil, false
	}
	fields := make(map[string]string, len(cols))
	rest := chunk[len(prefix):]
	for i, col := range cols {
		value := rest
		if i+1 < len(cols) {
			marker := ", " + cols[i+1] + "="
			end := strings.Index(rest, marker)
			if end < 0 {
				return nil, false
			}
			value = rest[:end]
			rest = rest[end+len(marker):]
		}
		if value == "NULL" {
			value = ""
		}
		fields[col] = value
	}
	return fields, true
}

func parseID(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n, err == nil
}
