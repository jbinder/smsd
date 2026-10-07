// Package ui serves the SMS conversation viewer over loopback HTTP and opens it
// in the user's browser. Its only writes are sending an SMS and marking what
// the reader has seen as read. A tiny embedded single-page app keeps the
// idle daemon lightweight: no GUI toolkit is linked and the server only runs
// while the viewer is open.
package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jbinder/smsd/internal/database"
	"github.com/jbinder/smsd/internal/logging"
)

//go:embed assets/index.html
var assets embed.FS

// Sender sends SMS on the viewer's behalf.
type Sender interface {
	// SendStatus returns nil when sending can work, or why it cannot.
	SendStatus() error
	// Send sends body to address and returns the address actually used.
	Send(ctx context.Context, address, body string) (string, error)
}

// Server is the loopback web viewer. It starts lazily on first Open.
type Server struct {
	db     *database.DB
	log    *logging.Logger
	addr   string
	sender Sender
	// onRead runs after the viewer marked something read, so the tray can
	// drop its unread indicator.
	onRead func()
	// token authorises sends. It is generated per run and only handed out
	// inside the page, which other sites cannot read.
	token string

	mu      sync.Mutex
	srv     *http.Server
	url     string
	running bool
}

// New creates a viewer server bound (on start) to addr, e.g. "127.0.0.1:0".
func New(db *database.DB, addr string, log *logging.Logger) *Server {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return &Server{db: db, log: log, addr: addr, token: hex.EncodeToString(buf)}
}

// SetSender enables sending from the viewer.
func (s *Server) SetSender(sender Sender) { s.sender = sender }

// SetOnRead registers fn to run whenever the viewer marks messages or calls
// read.
func (s *Server) SetOnRead(fn func()) { s.onRead = fn }

// Open ensures the server is running and launches the browser at its URL.
func (s *Server) Open() error {
	url, err := s.start()
	if err != nil {
		return err
	}
	return openBrowser(url)
}

// start brings the server up if needed and returns its base URL.
func (s *Server) start() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return s.url, nil
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return "", fmt.Errorf("listening on %s: %w", s.addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/conversations", s.handleConversations)
	mux.HandleFunc("/api/messages", s.handleMessages)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/contacts", s.handleContacts)
	mux.HandleFunc("/api/contact", s.handleContact)
	mux.HandleFunc("/api/calls", s.handleCalls)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/read", s.handleRead)

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.url = "http://" + ln.Addr().String() + "/"
	s.running = true

	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Errorf("ui server: %v", err)
		}
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	s.log.Infof("viewer available at %s", s.url)
	return s.url, nil
}

// Stop shuts the viewer down (called on daemon exit).
func (s *Server) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "ui asset missing", http.StatusInternalServerError)
		return
	}
	data = bytes.Replace(data, []byte("%SMSD_TOKEN%"), []byte(s.token), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is embedded in the binary, so an upgrade changes it while the URL
	// stays put. Without this a cached copy keeps serving the old viewer.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// viewerState is the database fingerprint plus whether sending works right
// now, so the compose box can say why it is disabled.
type viewerState struct {
	database.State
	CanSend   bool   `json:"can_send"`
	SendError string `json:"send_error,omitempty"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state, err := s.db.State()
	v := viewerState{State: state}
	if sendErr := s.sendStatus(); sendErr != nil {
		v.SendError = sendErr.Error()
	} else {
		v.CanSend = true
	}
	s.writeJSON(w, v, err)
}

func (s *Server) sendStatus() error {
	if s.sender == nil {
		return fmt.Errorf("sending is not available")
	}
	return s.sender.SendStatus()
}

// sendTimeout bounds a send. It is not tied to the request: a browser that
// goes away mid-send must not cut a multipart message short.
const sendTimeout = 90 * time.Second

// handleSend sends one SMS. It costs money, so it only accepts requests the viewer page itself made — see
// authorised.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorised(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Address string `json:"address"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Address) == "" || strings.TrimSpace(req.Body) == "" {
		http.Error(w, "recipient and message are required", http.StatusBadRequest)
		return
	}
	if err := s.sendStatus(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	sentTo, err := s.sender.Send(ctx, req.Address, req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.writeJSON(w, struct {
		Address string `json:"address"`
	}{sentTo}, nil)
}

// handleRead marks what the viewer has shown as read: one conversation's
// messages, or the call log, up to the newest item rendered (until, epoch ms).
// The page decides when the reader has actually seen them.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorised(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Address string `json:"address"`
		Calls   bool   `json:"calls"`
		Until   int64  `json:"until"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if (req.Address == "") == !req.Calls || req.Until <= 0 {
		http.Error(w, "one of address or calls, and until, are required", http.StatusBadRequest)
		return
	}
	var n int64
	var err error
	if req.Calls {
		n, err = s.db.MarkCallsRead(req.Until)
	} else {
		n, err = s.db.MarkConversationRead(req.Address, req.Until)
	}
	if err == nil && n > 0 && s.onRead != nil {
		s.onRead()
	}
	s.writeJSON(w, struct {
		Marked int64 `json:"marked"`
	}{n}, err)
}

// authorised accepts a write only from the viewer page. Any site open in the
// user's browser can make it POST to a loopback port, so:
//   - the page's per-run token must come back in a header. Other origins
//     cannot read the page to learn it, and a custom header also forces a
//     CORS preflight, which this server never approves;
//   - Host must be loopback, which defeats DNS rebinding — a hostile name
//     re-pointed at 127.0.0.1 would otherwise be same-origin with us;
//   - Origin, which browsers send on POST, must be loopback too.
func (s *Server) authorised(r *http.Request) bool {
	token := r.Header.Get("X-Smsd-Token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		return false
	}
	if !isLoopbackHost(r.Host) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !isLoopbackHost(u.Host) {
			return false
		}
	}
	return true
}

func isLoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	convs, err := s.db.Conversations(
		sinceParam(r),
		intParam(r, "limit", 100, 1000),
		intParam(r, "offset", 0, 1<<30),
	)
	s.writeJSON(w, convs, err)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("address")
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	cur := database.Cursor{
		Date:      int64(intParam(r, "before_date", 0, 1<<62)),
		AndroidID: int64(intParam(r, "before_id", 0, 1<<62)),
	}
	page, err := s.db.Messages(addr, sinceParam(r), cur, intParam(r, "limit", 200, 1000))
	s.writeJSON(w, page, err)
}

// handleCalls pages the call log, newest first, optionally for one number.
// It takes the same window and cursor parameters as handleMessages.
func (s *Server) handleCalls(w http.ResponseWriter, r *http.Request) {
	cur := database.Cursor{
		Date:      int64(intParam(r, "before_date", 0, 1<<62)),
		AndroidID: int64(intParam(r, "before_id", 0, 1<<62)),
	}
	page, err := s.db.Calls(r.URL.Query().Get("number"), sinceParam(r), cur,
		intParam(r, "limit", 200, 1000))
	s.writeJSON(w, page, err)
}

// sinceParam reads the millisecond-epoch window floor shared by the list and
// thread endpoints. Absent or zero means the whole history.
func sinceParam(r *http.Request) int64 {
	return int64(intParam(r, "since", 0, 1<<62))
}

// intParam reads a non-negative integer query parameter, falling back to def
// when absent or unparseable and clamping to max so a hand-edited URL cannot
// ask the server to materialise the whole table.
func intParam(r *http.Request, name string, def, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		s.writeJSON(w, []struct{}{}, nil)
		return
	}
	msgs, err := s.db.Search(q)
	s.writeJSON(w, msgs, err)
}

// handleContacts lists every contact, deleted ones included; there are few
// enough that the viewer filters them itself.
func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := s.db.Contacts()
	s.writeJSON(w, contacts, err)
}

func (s *Server) handleContact(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	card, ok, err := s.db.Contact(id)
	if err == nil && !ok {
		http.NotFound(w, r)
		return
	}
	s.writeJSON(w, card, err)
}

func (s *Server) writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		s.log.Errorf("ui query: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Errorf("ui encode: %v", err)
	}
}

// openBrowser launches the default browser without blocking on it.
//
// A plain child process would inherit the daemon's cgroup, and the systemd
// unit caps that at a few tens of megabytes to keep the daemon honest. A
// browser blows through the cap within seconds and the OOM killer takes the
// browser and the daemon down together. Under systemd the browser is therefore
// started in a transient scope of its own, which moves it out from under the
// daemon's limit while keeping the daemon's environment (DISPLAY, session bus).
func openBrowser(url string) error {
	var cmd *exec.Cmd
	if os.Getenv("INVOCATION_ID") != "" {
		if run, err := exec.LookPath("systemd-run"); err == nil {
			cmd = exec.Command(run, "--user", "--scope", "--quiet", "--collect",
				"--unit=smsd-viewer-"+strconv.FormatInt(time.Now().UnixMilli(), 36),
				"xdg-open", url)
		}
	}
	if cmd == nil {
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the launcher so it does not linger as a zombie; the browser itself
	// outlives it.
	go cmd.Wait()
	return nil
}
