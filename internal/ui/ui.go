// Package ui serves a read-only SMS conversation viewer over loopback HTTP and
// opens it in the user's browser. A tiny embedded single-page app keeps the
// idle daemon lightweight: no GUI toolkit is linked and the server only runs
// while the viewer is open.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/jbinder/smsd/internal/database"
	"github.com/jbinder/smsd/internal/logging"
)

//go:embed assets/index.html
var assets embed.FS

// Server is the loopback web viewer. It starts lazily on first Open.
type Server struct {
	db   *database.DB
	log  *logging.Logger
	addr string

	mu      sync.Mutex
	srv     *http.Server
	url     string
	running bool
}

// New creates a viewer server bound (on start) to addr, e.g. "127.0.0.1:0".
func New(db *database.DB, addr string, log *logging.Logger) *Server {
	return &Server{db: db, log: log, addr: addr}
}

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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is embedded in the binary, so an upgrade changes it while the URL
	// stays put. Without this a cached copy keeps serving the old viewer.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state, err := s.db.State()
	s.writeJSON(w, state, err)
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
func openBrowser(url string) error {
	cmd := exec.Command("xdg-open", url)
	return cmd.Start()
}
