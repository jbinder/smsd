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
	w.Write(data)
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	convs, err := s.db.Conversations()
	s.writeJSON(w, convs, err)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("address")
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	msgs, err := s.db.Messages(addr)
	s.writeJSON(w, msgs, err)
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
