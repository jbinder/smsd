package sms

import "testing"

func TestParseQueryOutput_Basic(t *testing.T) {
	raw := "Row: 0 _id=1, thread_id=5, address=+15551234567, date=1600000000000, date_sent=1599999999000, type=1, read=1, body=Hello there\n" +
		"Row: 1 _id=2, thread_id=5, address=+15551234567, date=1600000005000, date_sent=1600000004000, type=2, read=1, body=Hi back"

	msgs := ParseQueryOutput(raw)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	m0 := msgs[0]
	if m0.AndroidID != 1 || m0.ThreadID != 5 || m0.Address != "+15551234567" {
		t.Errorf("row0 header parsed wrong: %+v", m0)
	}
	if m0.Date != 1600000000000 || m0.DateSent != 1599999999000 {
		t.Errorf("row0 dates parsed wrong: %+v", m0)
	}
	if m0.Type != TypeReceived || !m0.Received() || !m0.Read {
		t.Errorf("row0 type/read wrong: %+v", m0)
	}
	if m0.Body != "Hello there" {
		t.Errorf("row0 body = %q", m0.Body)
	}
	if msgs[1].Type != TypeSent || msgs[1].Received() {
		t.Errorf("row1 should be sent: %+v", msgs[1])
	}
}

func TestParseQueryOutput_BodyWithCommasAndEquals(t *testing.T) {
	// Body deliberately contains commas and '=' which naive splitting breaks on.
	raw := "Row: 0 _id=42, thread_id=3, address=Bank, date=1700000000000, date_sent=1700000000000, type=1, read=0, body=Your code is 12,345 and total=100, thanks"
	msgs := ParseQueryOutput(raw)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	want := "Your code is 12,345 and total=100, thanks"
	if msgs[0].Body != want {
		t.Errorf("body = %q, want %q", msgs[0].Body, want)
	}
	if msgs[0].Read {
		t.Errorf("expected unread")
	}
}

func TestParseQueryOutput_MultilineBody(t *testing.T) {
	// A body that spans multiple physical lines must stay attached to its row.
	raw := "Row: 0 _id=7, thread_id=1, address=+1, date=1, date_sent=1, type=1, read=1, body=line one\nline two\nline three\n" +
		"Row: 1 _id=8, thread_id=1, address=+1, date=2, date_sent=2, type=1, read=1, body=single"
	msgs := ParseQueryOutput(raw)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	want := "line one\nline two\nline three"
	if msgs[0].Body != want {
		t.Errorf("multiline body = %q, want %q", msgs[0].Body, want)
	}
	if msgs[1].Body != "single" {
		t.Errorf("row1 body = %q", msgs[1].Body)
	}
}

func TestParseQueryOutput_NullFields(t *testing.T) {
	raw := "Row: 0 _id=9, thread_id=NULL, address=NULL, date=1600000000000, date_sent=NULL, type=1, read=0, body=NULL"
	msgs := ParseQueryOutput(raw)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.ThreadID != 0 || m.Address != "" || m.DateSent != 0 || m.Body != "" {
		t.Errorf("NULL handling wrong: %+v", m)
	}
}

func TestParseQueryOutput_Empty(t *testing.T) {
	for _, in := range []string{"", "   \n", "No result found."} {
		if got := ParseQueryOutput(in); got != nil {
			t.Errorf("ParseQueryOutput(%q) = %v, want nil", in, got)
		}
	}
}

func TestParseQueryOutput_SkipsMalformed(t *testing.T) {
	// Second row has no _id and must be skipped without dropping valid rows.
	raw := "Row: 0 _id=1, thread_id=1, address=+1, date=1, date_sent=1, type=1, read=1, body=ok\n" +
		"Row: 1 thread_id=1, address=+1, date=1, date_sent=1, type=1, read=1, body=noid\n" +
		"Row: 2 _id=3, thread_id=1, address=+1, date=1, date_sent=1, type=2, read=1, body=also ok"
	msgs := ParseQueryOutput(raw)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 valid messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].AndroidID != 1 || msgs[1].AndroidID != 3 {
		t.Errorf("wrong ids survived: %+v", msgs)
	}
}

func TestProjectionArg(t *testing.T) {
	got := ProjectionArg()
	want := "_id:thread_id:address:date:date_sent:type:read:body"
	if got != want {
		t.Errorf("ProjectionArg() = %q, want %q", got, want)
	}
}
