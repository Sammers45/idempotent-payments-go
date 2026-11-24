// Package store persists payments and idempotency keys in SQLite.
//
// # How concurrent duplicates are prevented
//
// We use a database UNIQUE constraint (the idempotency_keys primary key) plus a
// transaction, not an in-memory per-key lock. An in-memory lock only protects
// one process: run two instances behind a load balancer and two retries can
// land on different instances and both charge. The database is the one place
// every instance shares, so that is where the guarantee has to live.
//
// Each create runs in one transaction that (1) looks up the key, (2) inserts
// the payment and (3) inserts the key row with the saved response. Either
// both rows are written or neither is, so we never have a payment without its
// key or a key without its payment.
//
// Transactions start with BEGIN IMMEDIATE (_txlock=immediate), which takes
// SQLite's write lock up front, so two requests with the same key run one after
// the other and the second sees the first's row. The primary key is still the
// real guarantee: if two inserts ever do collide (for example on a database
// with weaker locking, like Postgres at READ COMMITTED), the loser gets a
// constraint error, we retry once, and the retry finds and replays the
// winner's saved response.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/Sammers45/idempotent-payments-go/internal/payments"
)

// KeyTTL is how long an idempotency key is remembered. 24 hours matches
// Stripe's documented behavior and comfortably covers client retry windows.
const KeyTTL = 24 * time.Hour

var (
	// ErrKeyReused means the idempotency key was already used with a
	// different request body.
	ErrKeyReused = errors.New("idempotency key reused with a different request")
	// ErrNotFound means no payment has the given ID.
	ErrNotFound = errors.New("not found")
)

// SavedResponse is the HTTP response stored with an idempotency key, so a
// retry gets back exactly what the first request got.
type SavedResponse struct {
	Status int
	Body   []byte
}

// Store is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time // replaced in tests to simulate the 24h expiry
}

const schema = `
CREATE TABLE IF NOT EXISTS payments (
	id          TEXT PRIMARY KEY,
	amount      INTEGER NOT NULL CHECK (amount > 0),
	currency    TEXT    NOT NULL,
	customer_id TEXT    NOT NULL,
	status      TEXT    NOT NULL,
	created     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
	key             TEXT PRIMARY KEY,   -- the UNIQUE constraint that stops double charges
	request_hash    TEXT    NOT NULL,   -- SHA-256 of the normalized request
	response_status INTEGER NOT NULL,
	response_body   BLOB    NOT NULL,
	payment_id      TEXT    NOT NULL REFERENCES payments(id),
	expires_at      INTEGER NOT NULL    -- Unix seconds
);

CREATE INDEX IF NOT EXISTS idempotency_keys_expires_at ON idempotency_keys(expires_at);
`

// Open opens (or creates) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	// busy_timeout: wait up to 5s for the write lock instead of failing at
	// once with SQLITE_BUSY when another request holds it.
	// journal_mode=WAL: readers don't block the writer and vice versa.
	// _txlock=immediate: every BeginTx issues BEGIN IMMEDIATE (see package doc).
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// CreatePayment saves p together with its idempotency key and the response to
// replay, all in one transaction.
//
// If key was already used (and has not expired) with the same fingerprint, it
// returns the saved response with replayed=true and writes nothing. With a
// different fingerprint it returns ErrKeyReused.
func (s *Store) CreatePayment(ctx context.Context, key, fingerprint string, p payments.Payment, resp SavedResponse) (saved SavedResponse, replayed bool, err error) {
	saved, replayed, err = s.createOnce(ctx, key, fingerprint, p, resp)
	if isUniqueViolation(err) {
		// Lost a race with a concurrent request using the same key. Its
		// transaction has committed, so a second attempt will find the key
		// and replay (or report a mismatch).
		saved, replayed, err = s.createOnce(ctx, key, fingerprint, p, resp)
	}
	return saved, replayed, err
}

func (s *Store) createOnce(ctx context.Context, key, fingerprint string, p payments.Payment, resp SavedResponse) (SavedResponse, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SavedResponse{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	now := s.now()

	var (
		storedHash string
		storedResp SavedResponse
		expiresAt  int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT request_hash, response_status, response_body, expires_at
		   FROM idempotency_keys WHERE key = ?`, key,
	).Scan(&storedHash, &storedResp.Status, &storedResp.Body, &expiresAt)

	switch {
	case err == nil && now.Unix() < expiresAt:
		// Key is live: this is a retry or a misuse of the key.
		if storedHash != fingerprint {
			return SavedResponse{}, false, ErrKeyReused
		}
		return storedResp, true, nil

	case err == nil:
		// Key has expired: forget it and treat this as a brand-new request.
		if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE key = ?`, key); err != nil {
			return SavedResponse{}, false, fmt.Errorf("delete expired key: %w", err)
		}

	case errors.Is(err, sql.ErrNoRows):
		// First time we've seen this key.

	default:
		return SavedResponse{}, false, fmt.Errorf("look up key: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO payments (id, amount, currency, customer_id, status, created)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.Amount, p.Currency, p.CustomerID, p.Status, p.Created,
	); err != nil {
		return SavedResponse{}, false, fmt.Errorf("insert payment: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, request_hash, response_status, response_body, payment_id, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		key, fingerprint, resp.Status, resp.Body, p.ID, now.Add(KeyTTL).Unix(),
	); err != nil {
		// Wrap with %w so isUniqueViolation can still see the driver error.
		return SavedResponse{}, false, fmt.Errorf("insert idempotency key: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return SavedResponse{}, false, fmt.Errorf("commit: %w", err)
	}
	return resp, false, nil
}

// GetPayment returns the payment with the given ID, or ErrNotFound.
func (s *Store) GetPayment(ctx context.Context, id string) (payments.Payment, error) {
	var p payments.Payment
	err := s.db.QueryRowContext(ctx,
		`SELECT id, amount, currency, customer_id, status, created FROM payments WHERE id = ?`, id,
	).Scan(&p.ID, &p.Amount, &p.Currency, &p.CustomerID, &p.Status, &p.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return payments.Payment{}, ErrNotFound
	}
	if err != nil {
		return payments.Payment{}, fmt.Errorf("get payment: %w", err)
	}
	return p, nil
}

// CountPayments returns the number of payments. Tests use it to prove that
// retries never create a second row.
func (s *Store) CountPayments(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM payments`).Scan(&n)
	return n, err
}

// DeleteExpiredKeys removes idempotency keys past their expiry and returns how
// many it removed. Expired keys are already ignored on lookup; this just
// keeps the table from growing forever. The payments themselves are kept.
func (s *Store) DeleteExpiredKeys(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= ?`, s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("delete expired keys: %w", err)
	}
	return res.RowsAffected()
}

// isUniqueViolation reports whether err is SQLite rejecting a duplicate
// primary key or unique value.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code()
	return code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || code == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}