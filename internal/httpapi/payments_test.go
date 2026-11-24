package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Sammers45/idempotent-payments-go/internal/payments"
	"github.com/Sammers45/idempotent-payments-go/internal/store"
)

const validBody = `{"amount": 1000, "currency": "usd", "customer_id": "cus_123"}`

type result struct {
	status   int
	body     []byte
	replayed bool
}

// doPost sends a create request. It returns errors instead of calling
// t.Fatal so it is safe to use from goroutines (t.Fatal must only be called
// from the test's own goroutine).
func doPost(baseURL, key, body string) (result, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/payments", strings.NewReader(body))
	if err != nil {
		return result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(IdempotencyKeyHeader, key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{}, err
	}
	return result{status: resp.StatusCode, body: b, replayed: resp.Header.Get(ReplayedHeader) == "true"}, nil
}

func post(t *testing.T, baseURL, key, body string) result {
	t.Helper()
	r, err := doPost(baseURL, key, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func decodeError(t *testing.T, body []byte) apiError {
	t.Helper()
	var env struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not an error envelope: %s", body)
	}
	if env.Error.Type == "" || env.Error.Code == "" || env.Error.Message == "" {
		t.Fatalf("error must have type, code and message: %s", body)
	}
	return env.Error
}

func paymentCount(t *testing.T, st *store.Store) int {
	t.Helper()
	n, err := st.CountPayments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateAndGetPayment(t *testing.T) {
	ts, _ := newTestServer(t)

	r := post(t, ts.URL, "key-1", validBody)
	if r.status != http.StatusCreated || r.replayed {
		t.Fatalf("status=%d replayed=%v body=%s", r.status, r.replayed, r.body)
	}
	var created payments.Payment
	if err := json.Unmarshal(r.body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Amount != 1000 || created.Currency != "usd" || created.Status != "succeeded" {
		t.Fatalf("unexpected payment %+v", created)
	}

	resp, err := http.Get(ts.URL + "/v1/payments/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	var fetched payments.Payment
	if err := json.NewDecoder(resp.Body).Decode(&fetched); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || fetched != created {
		t.Fatalf("GET status=%d got %+v, want %+v", resp.StatusCode, fetched, created)
	}
}

func TestGetPaymentNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/payments/pay_nope")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(t, resp)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if e := decodeError(t, b); e.Code != "payment_not_found" {
		t.Fatalf("code = %q", e.Code)
	}
}

func TestRetryReturnsOriginalResponse(t *testing.T) {
	ts, st := newTestServer(t)

	first := post(t, ts.URL, "key-1", validBody)
	retry := post(t, ts.URL, "key-1", validBody)

	if retry.status != first.status || string(retry.body) != string(first.body) {
		t.Fatalf("retry = %d %s, want %d %s", retry.status, retry.body, first.status, first.body)
	}
	if !retry.replayed {
		t.Fatal("retry should carry Idempotent-Replayed: true")
	}
	if n := paymentCount(t, st); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

func TestRetryWithReformattedBodyIsStillARetry(t *testing.T) {
	ts, st := newTestServer(t)

	post(t, ts.URL, "key-1", validBody)
	// Same request: keys reordered, currency uppercased, no spaces.
	r := post(t, ts.URL, "key-1", `{"customer_id":"cus_123","currency":"USD","amount":1000}`)
	if r.status != http.StatusCreated || !r.replayed {
		t.Fatalf("status=%d replayed=%v, want a replayed 201", r.status, r.replayed)
	}
	if n := paymentCount(t, st); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

func TestSameKeyDifferentBodyIs422(t *testing.T) {
	ts, st := newTestServer(t)

	post(t, ts.URL, "key-1", validBody)
	r := post(t, ts.URL, "key-1", `{"amount": 2000, "currency": "usd", "customer_id": "cus_123"}`)

	if r.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", r.status)
	}
	e := decodeError(t, r.body)
	if e.Type != "idempotency_error" || e.Code != "idempotency_key_reused" {
		t.Fatalf("error = %+v", e)
	}
	if n := paymentCount(t, st); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

// The headline test: 50 identical requests at the same instant produce
// exactly one payment, and every caller sees the same response.
func TestConcurrentRetriesCreateExactlyOnePayment(t *testing.T) {
	ts, st := newTestServer(t)

	const n = 50
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]result, n)
		errs    = make([]error, n)
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // all goroutines fire together
			results[i], errs[i] = doPost(ts.URL, "same-key", validBody)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	replays := 0
	for i, r := range results {
		if r.status != http.StatusCreated {
			t.Fatalf("request %d: status %d body %s", i, r.status, r.body)
		}
		if string(r.body) != string(results[0].body) {
			t.Fatalf("request %d got a different body:\n%s\nvs\n%s", i, r.body, results[0].body)
		}
		if r.replayed {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("replays = %d, want %d", replays, n-1)
	}
	if got := paymentCount(t, st); got != 1 {
		t.Fatalf("payments = %d, want exactly 1", got)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing key", "", validBody, 400, "idempotency_key_missing"},
		{"key too long", strings.Repeat("k", 256), validBody, 400, "idempotency_key_too_long"},
		{"zero amount", "k", `{"amount":0,"currency":"usd","customer_id":"c"}`, 400, "amount_invalid"},
		{"negative amount", "k", `{"amount":-1,"currency":"usd","customer_id":"c"}`, 400, "amount_invalid"},
		{"float amount", "k", `{"amount":10.5,"currency":"usd","customer_id":"c"}`, 400, "body_invalid"},
		{"string amount", "k", `{"amount":"1000","currency":"usd","customer_id":"c"}`, 400, "body_invalid"},
		{"unsupported currency", "k", `{"amount":1,"currency":"xyz","customer_id":"c"}`, 400, "currency_unsupported"},
		{"missing currency", "k", `{"amount":1,"customer_id":"c"}`, 400, "currency_missing"},
		{"missing customer", "k", `{"amount":1,"currency":"usd"}`, 400, "customer_id_missing"},
		{"unknown field", "k", `{"ammount":1,"currency":"usd","customer_id":"c"}`, 400, "body_invalid"},
		{"not json", "k", `amount=1000`, 400, "body_invalid"},
		{"trailing data", "k", validBody + `{}`, 400, "body_invalid"},
		{"too large", "k", `{"customer_id":"` + strings.Repeat("x", 70_000) + `"}`, 413, "body_too_large"},
	}

	ts, st := newTestServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := post(t, ts.URL, tt.key, tt.body)
			if r.status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", r.status, tt.wantStatus, r.body)
			}
			if e := decodeError(t, r.body); e.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
	if n := paymentCount(t, st); n != 0 {
		t.Fatalf("invalid requests created %d payments", n)
	}
}

// A rejected request must not use up the key: the client fixes the body and
// retries with the same key.
func TestValidationErrorDoesNotConsumeKey(t *testing.T) {
	ts, _ := newTestServer(t)

	bad := post(t, ts.URL, "key-1", `{"amount":0,"currency":"usd","customer_id":"c"}`)
	if bad.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", bad.status)
	}
	good := post(t, ts.URL, "key-1", validBody)
	if good.status != http.StatusCreated || good.replayed {
		t.Fatalf("status=%d replayed=%v, want a fresh 201", good.status, good.replayed)
	}
}