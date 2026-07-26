// Package sms defines the SMS data model and the parser for the output of
// Android's `content query` command. Parsing is kept free of any I/O or adb
// dependency so it can be unit-tested in isolation.
package sms

import (
	"regexp"
	"strconv"
	"strings"
)

// Message type constants as used by Android's content://sms "type" column.
const (
	TypeReceived = 1 // MESSAGE_TYPE_INBOX
	TypeSent     = 2 // MESSAGE_TYPE_SENT
)

// Projection is the ordered list of columns smsd requests. "body" MUST remain
// last: it is the only free-text column and the parser treats everything after
// "body=" (including embedded commas and newlines) as the message body.
var Projection = []string{
	"_id", "thread_id", "address", "date", "date_sent", "type", "read", "body",
}

// ProjectionArg returns the colon-joined projection string content query wants.
func ProjectionArg() string { return strings.Join(Projection, ":") }

// Message is a single SMS as imported from a device.
type Message struct {
	AndroidID int64  // _id on the device (unique per device)
	ThreadID  int64  // thread_id
	Address   string // phone number / short code
	Body      string // message text
	Date      int64  // received/stored time, epoch milliseconds
	DateSent  int64  // sender timestamp, epoch milliseconds
	Type      int    // TypeReceived, TypeSent, ...
	Read      bool   // read flag on the device
}

// Received reports whether the message was received (as opposed to sent).
func (m Message) Received() bool { return m.Type == TypeReceived }

// rowSplit matches the "Row: <n> " prefix that content query emits at the start
// of every record. Using multiline mode lets us split records even when a body
// spans multiple physical lines.
var rowSplit = regexp.MustCompile(`(?m)^Row: \d+ `)

// ParseQueryOutput parses the stdout of
//
//	content query --uri content://sms --projection <Projection> ...
//
// into messages. Malformed rows are skipped rather than failing the batch, so
// one corrupt record cannot stall importing. The output is expected to use the
// Projection column order defined above.
func ParseQueryOutput(raw string) []Message {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "No result found") {
		return nil
	}

	chunks := rowSplit.Split(raw, -1)
	msgs := make([]Message, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = strings.TrimRight(chunk, "\r\n")
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		if m, ok := parseRow(chunk); ok {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

// parseRow parses a single record body (with the "Row: N " prefix already
// stripped). It relies on "body" being the final projected column: the head
// fields are comma-separated key=value pairs, and the body is the remainder.
func parseRow(chunk string) (Message, bool) {
	const bodyKey = "body="
	fields := map[string]string{}
	var body string

	if idx := strings.Index(chunk, bodyKey); idx >= 0 {
		body = chunk[idx+len(bodyKey):]
		head := strings.TrimRight(chunk[:idx], ", ")
		parseHead(head, fields)
	} else {
		// No body column present (e.g. a different projection); parse it all.
		parseHead(chunk, fields)
	}

	// _id is mandatory: without it we cannot deduplicate.
	idStr, ok := fields["_id"]
	if !ok {
		return Message{}, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil {
		return Message{}, false
	}

	m := Message{
		AndroidID: id,
		ThreadID:  parseInt(fields["thread_id"]),
		Address:   cleanText(fields["address"]),
		Body:      normalizeBody(body),
		Date:      parseInt(fields["date"]),
		DateSent:  parseInt(fields["date_sent"]),
		Type:      int(parseInt(fields["type"])),
		Read:      parseInt(fields["read"]) != 0,
	}
	return m, true
}

// parseHead splits "k=v, k=v, ..." pairs into the fields map.
func parseHead(head string, fields map[string]string) {
	for _, part := range strings.Split(head, ", ") {
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(part[:eq])
		val := part[eq+1:]
		fields[key] = val
	}
}

// parseInt parses a possibly-NULL integer column, returning 0 on any problem.
func parseInt(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NULL" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// cleanText normalises a simple text column, mapping NULL to empty.
func cleanText(s string) string {
	s = strings.TrimSpace(s)
	if s == "NULL" {
		return ""
	}
	return s
}

// normalizeBody trims a trailing newline that content query appends without
// disturbing intentional internal whitespace, and maps a literal NULL body.
func normalizeBody(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if s == "NULL" {
		return ""
	}
	return s
}
