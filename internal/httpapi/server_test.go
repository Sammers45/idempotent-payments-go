package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sammers45/idempotent-payments-go/internal/store"
)

// newTestServer starts a real HTTP server backed by a fresh SQLite file, so
// tests go through routing, middleware, handlers and the database.
func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(NewServer(logger, st).Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return ts, st
}

// closeBody closes a response body and fails the test if that errors.
func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
}

func TestHealthz(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body = %v, want status ok", body)
	}
}

func TestEveryResponseHasUniqueRequestID(t *testing.T) {
	ts, _ := newTestServer(t)

	seen := map[string]bool{}
	// Include a 404 to show the ID is set even when no route matches.
	for _, path := range []string{"/healthz", "/healthz", "/does-not-exist"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		closeBody(t, resp)

		id := resp.Header.Get(RequestIDHeader)
		if !strings.HasPrefix(id, "req_") {
			t.Fatalf("%s: request ID = %q, want req_ prefix", path, id)
		}
		if seen[id] {
			t.Fatalf("duplicate request ID %q", id)
		}
		seen[id] = true
	}
}

func TestHealthzRejectsWrongMethod(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/healthz", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}