// Command server runs the idempotent payments API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sammers45/idempotent-payments-go/internal/httpapi"
	"github.com/Sammers45/idempotent-payments-go/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	addr := envOr("ADDR", ":8080")
	dbPath := envOr("DB_PATH", "payments.db")

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	// Runs last (defers are LIFO), after the server has drained, so no
	// in-flight request is cut off from the database.
	defer func() { _ = st.Close() }()

	srv := &http.Server{
		Addr:    addr,
		Handler: httpapi.NewServer(logger, st).Handler(),
		// Without timeouts, a slow or malicious client can hold a connection
		// (and a goroutine) open forever. ReadHeaderTimeout blocks Slowloris.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// ctx is cancelled on SIGINT (Ctrl-C) or SIGTERM (what Kubernetes and
	// Docker send before killing a container).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go sweepExpiredKeys(ctx, logger, st, time.Hour)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err // failed to start, e.g. port already in use
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting new connections and let in-flight
	// requests finish, so a payment that is mid-write is not cut off.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("stopped")
	return nil
}

// sweepExpiredKeys deletes expired idempotency keys every interval until ctx
// is cancelled. Correctness doesn't depend on it (expired keys are ignored on
// lookup); it only stops the table from growing without bound.
func sweepExpiredKeys(ctx context.Context, logger *slog.Logger, st *store.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.DeleteExpiredKeys(ctx)
			if err != nil {
				logger.Error("sweep expired keys", "error", err)
				continue
			}
			logger.Info("swept expired idempotency keys", "deleted", n)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}