package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// RequestIDHeader is returned on every response so a caller can quote it in a
// support ticket and we can find the matching log lines.
const RequestIDHeader = "Request-Id"

type ctxKey struct{}

// RequestIDFrom returns the request ID stored in ctx, or "" if there is none.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// LoggerFrom returns a logger that already carries the request ID, so every
// log line written while handling a request can be tied back to it.
func LoggerFrom(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := RequestIDFrom(ctx); id != "" {
		return base.With("request_id", id)
	}
	return base
}

func newRequestID() string {
	// 16 random bytes = 128 bits: collisions are not a practical concern.
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms (Go 1.24+ panics instead).
	return "req_" + hex.EncodeToString(b)
}

// statusRecorder captures the status code so the access log can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// WithRequestID assigns every request a fresh ID, sets it on the response,
// stores it in the context, and writes one access-log line per request.
//
// We generate the ID ourselves rather than trusting an incoming header: a
// client-supplied value could be forged or collide with someone else's.
func WithRequestID(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), ctxKey{}, id)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))

		logger.Info("request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}