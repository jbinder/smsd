package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jbinder/smsd/internal/database"
	"github.com/jbinder/smsd/internal/logging"
	"github.com/jbinder/smsd/internal/sms"
)

// newTestServer returns a Server backed by a temp database holding n messages
// in one conversation, plus an httptest server in front of its routes.
func newTestServer(t *testing.T, n int) (*httptest.Server, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var msgs []sms.Message
	for i := 1; i <= n; i++ {
		msgs = append(msgs, sms.Message{
			AndroidID: int64(i), ThreadID: 1, Address: "+15551234567",
			Body: "message " + strconv.Itoa(i), Date: int64(i) * 1000, Type: sms.TypeReceived,
		})
	}
	if n > 0 {
		if _, err := db.ImportMessages("dev1", msgs, true); err != nil {
			t.Fatalf("importing: %v", err)
		}
	}

	log, err := logging.New(filepath.Join(dir, "log.txt"), 1<<20, nil)
	if err != nil {
		t.Fatalf("opening log: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	s := New(db, "127.0.0.1:0", log)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/conversations", s.handleConversations)
	mux.HandleFunc("/api/messages", s.handleMessages)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, db
}

func getJSON(t *testing.T, srv *httptest.Server, path string, into any) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

// A thread far larger than one page must come back one page at a time, so the
// viewer never has to render the whole history at once.
func TestMessagesEndpointPages(t *testing.T) {
	srv, _ := newTestServer(t, 500)

	var first database.MessagePage
	getJSON(t, srv, "/api/messages?address=%2B15551234567&limit=200", &first)
	if len(first.Messages) != 200 || !first.HasMore {
		t.Fatalf("first page: %d messages, has_more=%v; want 200 and true",
			len(first.Messages), first.HasMore)
	}
	// Chronological within the page, ending at the newest message.
	if first.Messages[len(first.Messages)-1].AndroidID != 500 {
		t.Errorf("page ends at android_id %d, want 500", first.Messages[len(first.Messages)-1].AndroidID)
	}

	oldest := first.Messages[0]
	var older database.MessagePage
	getJSON(t, srv, "/api/messages?address=%2B15551234567&limit=200"+
		"&before_date="+strconv.FormatInt(oldest.Date, 10)+"&before_id="+strconv.FormatInt(oldest.AndroidID, 10), &older)
	if len(older.Messages) != 200 {
		t.Fatalf("second page has %d messages, want 200", len(older.Messages))
	}
	if older.Messages[len(older.Messages)-1].AndroidID >= oldest.AndroidID {
		t.Errorf("second page overlaps the first: ends at %d, cursor was %d",
			older.Messages[len(older.Messages)-1].AndroidID, oldest.AndroidID)
	}
}

// The default limit must bound the response even when no limit is given.
func TestMessagesEndpointDefaultLimit(t *testing.T) {
	srv, _ := newTestServer(t, 500)
	var page database.MessagePage
	getJSON(t, srv, "/api/messages?address=%2B15551234567", &page)
	if len(page.Messages) != 200 {
		t.Errorf("default page has %d messages, want 200", len(page.Messages))
	}
}

func TestMessagesEndpointRequiresAddress(t *testing.T) {
	srv, _ := newTestServer(t, 1)
	resp, err := srv.Client().Get(srv.URL + "/api/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestConversationsEndpointLimitAndOffset(t *testing.T) {
	srv, db := newTestServer(t, 1)
	// A second conversation, newer than the first.
	if _, err := db.ImportMessages("dev1", []sms.Message{
		{AndroidID: 999, Address: "+15559999999", Body: "newer", Date: 9_000_000, Type: sms.TypeReceived},
	}, true); err != nil {
		t.Fatal(err)
	}

	var page []database.Conversation
	getJSON(t, srv, "/api/conversations?limit=1", &page)
	if len(page) != 1 || page[0].Address != "+15559999999" {
		t.Fatalf("limit=1 returned %+v, want just the newest conversation", page)
	}
	getJSON(t, srv, "/api/conversations?limit=1&offset=1", &page)
	if len(page) != 1 || page[0].Address != "+15551234567" {
		t.Fatalf("offset=1 returned %+v, want the older conversation", page)
	}
}

// A bad or oversized limit must fall back to a sane bound rather than erroring
// or letting the caller pull the whole table.
func TestIntParamFallsBackAndClamps(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?good=5&bad=abc&neg=-1&huge=999999", nil)
	cases := []struct {
		name string
		want int
	}{
		{"good", 5},
		{"bad", 100},
		{"neg", 100},
		{"huge", 1000},
		{"missing", 100},
	}
	for _, c := range cases {
		if got := intParam(r, c.name, 100, 1000); got != c.want {
			t.Errorf("intParam(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestStateEndpointTracksImports(t *testing.T) {
	srv, db := newTestServer(t, 3)

	var before database.State
	getJSON(t, srv, "/api/state", &before)
	if before.Count != 3 {
		t.Fatalf("count = %d, want 3", before.Count)
	}

	if _, err := db.ImportMessages("dev1", []sms.Message{
		{AndroidID: 4242, Address: "+15551234567", Body: "new", Date: 9_000_000, Type: sms.TypeReceived},
	}, true); err != nil {
		t.Fatal(err)
	}

	var after database.State
	getJSON(t, srv, "/api/state", &after)
	if after.Count != 4 || after.MaxID <= before.MaxID {
		t.Errorf("state did not advance: before %+v, after %+v", before, after)
	}
}

// Guard the escaping the viewer relies on: an address is put straight into a
// query string, so it must survive the round trip unchanged.
func TestMessagesEndpointEscapedAddress(t *testing.T) {
	srv, db := newTestServer(t, 1)
	const weird = "+1 (555) 000-0000"
	if _, err := db.ImportMessages("dev1", []sms.Message{
		{AndroidID: 77, Address: weird, Body: "hi", Date: 500, Type: sms.TypeReceived},
	}, true); err != nil {
		t.Fatal(err)
	}
	var page database.MessagePage
	getJSON(t, srv, "/api/messages?address="+url.QueryEscape(weird), &page)
	if len(page.Messages) != 1 || page.Messages[0].Address != weird {
		t.Errorf("got %+v, want the one message for %q", page.Messages, weird)
	}
}
