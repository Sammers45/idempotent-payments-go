package payments

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := CreateRequest{Amount: 1000, Currency: "usd", CustomerID: "cus_123"}

	tests := []struct {
		name     string
		mutate   func(r *CreateRequest)
		wantCode string // "" means valid
	}{
		{"valid", func(*CreateRequest) {}, ""},
		{"zero amount", func(r *CreateRequest) { r.Amount = 0 }, "amount_invalid"},
		{"negative amount", func(r *CreateRequest) { r.Amount = -5 }, "amount_invalid"},
		{"max amount ok", func(r *CreateRequest) { r.Amount = MaxAmount }, ""},
		{"over max", func(r *CreateRequest) { r.Amount = MaxAmount + 1 }, "amount_too_large"},
		{"missing currency", func(r *CreateRequest) { r.Currency = "" }, "currency_missing"},
		{"unsupported currency", func(r *CreateRequest) { r.Currency = "xyz" }, "currency_unsupported"},
		{"uppercase currency normalized", func(r *CreateRequest) { r.Currency = " USD " }, ""},
		{"missing customer", func(r *CreateRequest) { r.CustomerID = "  " }, "customer_id_missing"},
		{"customer too long", func(r *CreateRequest) { r.CustomerID = strings.Repeat("c", 256) }, "customer_id_too_long"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.mutate(&r)
			r.Normalize()
			err := r.Validate()

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %v", err)
			}
			if ve.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", ve.Code, tt.wantCode)
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	a := CreateRequest{Amount: 1000, Currency: "USD", CustomerID: "cus_1"}
	b := CreateRequest{Amount: 1000, Currency: "usd", CustomerID: " cus_1 "}
	a.Normalize()
	b.Normalize()
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("equivalent requests should have the same fingerprint")
	}

	c := a
	c.Amount = 1001
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatal("different amounts must have different fingerprints")
	}
}

func TestNewIDIsUniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := NewID()
		if !strings.HasPrefix(id, "pay_") || len(id) != len("pay_")+32 {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}