// Package httpapi holds the HTTP layer: routing, middleware and handlers.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Sammers45/idempotent-payments-go/internal/store"
)

// Server wires dependencies into HTTP handlers.
type Server struct {
	logger *slog.Logger
	store  *store.Store
	now    func() time.Time
}

// NewServer builds a Server.
func NewServer(logger *slog.Logger, st *store.Store) *Server {
	return &Server{logger: logger, store: st, now: time.Now}
}

// Handler returns the root handler with all routes and middleware attached.
// Go 1.22's ServeMux supports method + path patterns ("GET /healthz") and
// path parameters ("{id}"), so no third-party router is needed.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/payments", s.handleCreatePayment)
	mux.HandleFunc("GET /v1/payments/{id}", s.handleGetPayment)
	return WithRequestID(s.logger, mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client may have gone away; nothing useful to do on error
}