package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sammers45/idempotent-payments-go/internal/payments"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newPayment() payments.Payment {
	return payments.Payment{
		ID: payments.NewID(), Amount: 1000, Currency: "usd",
		CustomerID: "cus_1", Status: payments.StatusSucceeded, Created: 1,
	}
}

func count(t *testing.T, s *Store) int {
	t.Helper()
	n, err := s.CountPayments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var resp201 = SavedResponse{Status: 201, Body: []byte(`{"first":true}`)}

func TestCreateThenReplay(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	got, replayed, err := s.CreatePayment(ctx, "key-1", "hash-a", newPayment(), resp201)
	if err != nil || replayed || got.Status != 201 {
		t.Fatalf("first call: got=%v replayed=%v err=%v", got, replayed, err)
	}

	// The retry builds a new payment (new ID, different response body), but
	// must get the ORIGINAL saved response back and write nothing.
	retry := SavedResponse{Status: 201, Body: []byte(`{"second":true}`)}
	got, replayed, err = s.CreatePayment(ctx, "key-1", "hash-a", newPayment(), retry)
	if err != nil || !replayed {
		t.Fatalf("retry: replayed=%v err=%v", replayed, err)
	}
	if string(got.Body) != `{"first":true}` {
		t.Fatalf("retry body = %s, want the original", got.Body)
	}
	if n := count(t, s); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

func TestKeyReusedWithDifferentRequest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, _, err := s.CreatePayment(ctx, "key-1", "hash-a", newPayment(), resp201); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.CreatePayment(ctx, "key-1", "hash-B", newPayment(), resp201)
	if !errors.Is(err, ErrKeyReused) {
		t.Fatalf("err = %v, want ErrKeyReused", err)
	}
	if n := count(t, s); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

func TestKeyExpiresAfter24Hours(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return start }

	if _, _, err := s.CreatePayment(ctx, "key-1", "hash-a", newPayment(), resp201); err != nil {
		t.Fatal(err)
	}

	// One second before expiry: still a replay.
	s.now = func() time.Time { return start.Add(KeyTTL - time.Second) }
	if _, replayed, err := s.CreatePayment(ctx, "key-1", "hash-a", newPayment(), resp201); err != nil || !replayed {
		t.Fatalf("before expiry: replayed=%v err=%v", replayed, err)
	}

	// At expiry: the key is forgotten, so even a different body is accepted
	// as a new payment.
	s.now = func() time.Time { return start.Add(KeyTTL) }
	if _, replayed, err := s.CreatePayment(ctx, "key-1", "hash-B", newPayment(), resp201); err != nil || replayed {
		t.Fatalf("after expiry: replayed=%v err=%v", replayed, err)
	}
	if n := count(t, s); n != 2 {
		t.Fatalf("payments = %d, want 2", n)
	}
}

func TestDeleteExpiredKeys(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return start }

	for _, k := range []string{"a", "b"} {
		if _, _, err := s.CreatePayment(ctx, k, "h", newPayment(), resp201); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return start.Add(KeyTTL) }
	n, err := s.DeleteExpiredKeys(ctx)
	if err != nil || n != 2 {
		t.Fatalf("deleted=%d err=%v, want 2", n, err)
	}
	if c := count(t, s); c != 2 {
		t.Fatalf("payments = %d, want 2 (payments are never deleted)", c)
	}
}

func TestGetPayment(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	p := newPayment()
	if _, _, err := s.CreatePayment(ctx, "k", "h", p, resp201); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetPayment(ctx, p.ID)
	if err != nil || got != p {
		t.Fatalf("got %+v err=%v, want %+v", got, err, p)
	}
	if _, err := s.GetPayment(ctx, "pay_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestConcurrentSameKeyStore hammers the store directly (no HTTP) so a
// failure here points at the transaction logic, not the handler.
func TestConcurrentSameKeyStore(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const n = 50
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		created  int
		replays  int
		firstErr error
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines at once to maximize contention
			_, replayed, err := s.CreatePayment(ctx, "same-key", "h", newPayment(), resp201)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil && firstErr == nil:
				firstErr = err
			case err == nil && replayed:
				replays++
			case err == nil:
				created++
			}
		}()
	}
	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatal(firstErr)
	}
	if created != 1 || replays != n-1 {
		t.Fatalf("created=%d replays=%d, want 1 and %d", created, replays, n-1)
	}
	if c := count(t, s); c != 1 {
		t.Fatalf("payments = %d, want 1", c)
	}
}