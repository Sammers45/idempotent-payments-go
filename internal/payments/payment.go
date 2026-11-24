// Package payments holds the domain types: what a payment is, what a valid
// create request looks like, and how we fingerprint a request for idempotency.
// It knows nothing about HTTP or SQL.
package payments

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Amounts are int64 minor units (cents for USD). Floats can't represent most
// decimal fractions exactly (0.1 + 0.2 != 0.3), and rounding errors in money
// are bugs. Integers are exact.
//
// MaxAmount caps a single payment at 999,999.99 in the major unit, which also
// keeps sums far away from int64 overflow.
const MaxAmount int64 = 99_999_999

// supportedCurrencies lists ISO 4217 codes (lowercase, as Stripe uses) that we
// accept. All have two decimal places, so "amount in cents" holds for each.
// Zero-decimal currencies like JPY would need different handling.
var supportedCurrencies = map[string]bool{
	"usd": true,
	"eur": true,
	"gbp": true,
	"cad": true,
}

// Payment is the stored and returned representation of a payment.
type Payment struct {
	ID         string `json:"id"`
	Amount     int64  `json:"amount"`
	Currency   string `json:"currency"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
	Created    int64  `json:"created"` // Unix seconds, UTC
}

// StatusSucceeded is the only status for now: there is no real card network
// behind this service, so every valid payment "succeeds".
const StatusSucceeded = "succeeded"

// CreateRequest is the body of POST /v1/payments.
type CreateRequest struct {
	Amount     int64  `json:"amount"`
	Currency   string `json:"currency"`
	CustomerID string `json:"customer_id"`
}

// ValidationError is a client mistake in the request. Code is a stable,
// machine-readable string; Message is for humans.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Code + ": " + e.Message }

// Normalize puts the request in canonical form so that equivalent requests
// ("USD" vs "usd", stray spaces) are treated as the same request.
func (r *CreateRequest) Normalize() {
	r.Currency = strings.ToLower(strings.TrimSpace(r.Currency))
	r.CustomerID = strings.TrimSpace(r.CustomerID)
}

// Validate returns a *ValidationError describing the first problem found, or
// nil. Call Normalize first.
func (r *CreateRequest) Validate() error {
	switch {
	case r.Amount <= 0:
		return &ValidationError{"amount_invalid", "amount must be a positive integer in the currency's minor unit (e.g. cents)"}
	case r.Amount > MaxAmount:
		return &ValidationError{"amount_too_large", fmt.Sprintf("amount must be at most %d", MaxAmount)}
	case r.Currency == "":
		return &ValidationError{"currency_missing", "currency is required"}
	case !supportedCurrencies[r.Currency]:
		return &ValidationError{"currency_unsupported", fmt.Sprintf("currency %q is not supported", r.Currency)}
	case r.CustomerID == "":
		return &ValidationError{"customer_id_missing", "customer_id is required"}
	case len(r.CustomerID) > 255:
		return &ValidationError{"customer_id_too_long", "customer_id must be at most 255 characters"}
	}
	return nil
}

// Fingerprint is the SHA-256 of the normalized request, hex encoded. We store
// it next to the idempotency key; a later request with the same key but a
// different fingerprint is a client bug (key reuse), not a retry.
//
// We hash the parsed, normalized request rather than the raw bytes. A retry
// from a different client library may reorder keys or change whitespace, and
// that is still the same request. json.Marshal of a struct always emits
// fields in declaration order, so this is deterministic.
func (r CreateRequest) Fingerprint() string {
	b, _ := json.Marshal(r) // cannot fail: the struct holds only strings and ints
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NewID returns a random, prefixed payment ID such as "pay_3f9a...". The
// prefix makes IDs self-describing in logs; 128 random bits make collisions
// practically impossible without a central counter.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "pay_" + hex.EncodeToString(b)
}