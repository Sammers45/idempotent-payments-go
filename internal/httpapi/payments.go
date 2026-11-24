package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Sammers45/idempotent-payments-go/internal/payments"
	"github.com/Sammers45/idempotent-payments-go/internal/store"
)

const (
	// IdempotencyKeyHeader is the request header carrying the client's key.
	IdempotencyKeyHeader = "Idempotency-Key"
	// ReplayedHeader is set to "true" when a response is a replay of a saved
	// one, so a client can tell a retry hit the cache (Stripe does the same).
	ReplayedHeader = "Idempotent-Replayed"

	maxKeyLength = 255
	maxBodyBytes = 64 << 10 // 64 KiB: far more than a valid body needs
)

// handleCreatePayment implements POST /v1/payments.
//
// Order matters: we validate before touching the store, so a request that is
// rejected for bad input does NOT use up the idempotency key. The client can
// fix the body and retry with the same key. (Stripe behaves the same way:
// results are only saved once the request actually starts executing.)
func (s *Server) handleCreatePayment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := LoggerFrom(ctx, s.logger)

	key := r.Header.Get(IdempotencyKeyHeader)
	if key == "" {
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "idempotency_key_missing",
			"the Idempotency-Key header is required for POST /v1/payments")
		return
	}
	if len(key) > maxKeyLength {
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "idempotency_key_too_long",
			"the Idempotency-Key header must be at most 255 characters")
		return
	}

	req, ok := decodeCreateRequest(w, r)
	if !ok {
		return
	}
	req.Normalize()
	if err := req.Validate(); err != nil {
		var ve *payments.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, errTypeInvalidRequest, ve.Code, ve.Message)
			return
		}
		writeError(w, http.StatusInternalServerError, errTypeAPI, "internal_error", "internal error")
		return
	}

	// Build the payment and its response body now, so the exact bytes we send
	// are the bytes we save for replays.
	p := payments.Payment{
		ID:         payments.NewID(),
		Amount:     req.Amount,
		Currency:   req.Currency,
		CustomerID: req.CustomerID,
		Status:     payments.StatusSucceeded,
		Created:    s.now().Unix(),
	}
	body, err := json.Marshal(p)
	if err != nil {
		log.Error("marshal payment", "error", err)
		writeError(w, http.StatusInternalServerError, errTypeAPI, "internal_error", "internal error")
		return
	}

	saved, replayed, err := s.store.CreatePayment(ctx, key, req.Fingerprint(), p,
		store.SavedResponse{Status: http.StatusCreated, Body: body})
	switch {
	case errors.Is(err, store.ErrKeyReused):
		// 422: the request is well-formed but conflicts with how this key was
		// used before. Almost always a client bug (reusing keys across
		// different payments), so we refuse loudly instead of guessing.
		log.Warn("idempotency key reused with different body")
		writeError(w, http.StatusUnprocessableEntity, errTypeIdempotency, "idempotency_key_reused",
			"this Idempotency-Key was already used with a different request body")
		return
	case err != nil:
		log.Error("create payment", "error", err)
		writeError(w, http.StatusInternalServerError, errTypeAPI, "internal_error", "internal error")
		return
	}

	if replayed {
		w.Header().Set(ReplayedHeader, "true")
		log.Info("replayed saved response")
	} else {
		log.Info("payment created", "payment_id", p.ID, "amount", p.Amount, "currency", p.Currency)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(saved.Status)
	_, _ = w.Write(saved.Body)
}

// decodeCreateRequest parses the JSON body strictly. It writes a 400 and
// returns ok=false on any problem.
func decodeCreateRequest(w http.ResponseWriter, r *http.Request) (req payments.CreateRequest, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	// Reject unknown fields: a typo like "ammount" should be an error, not a
	// silently ignored field that leaves amount at 0.
	dec.DisallowUnknownFields()

	const msg = "request body must be a JSON object with integer amount, currency and customer_id"
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, errTypeInvalidRequest, "body_too_large", "request body is too large")
			return req, false
		}
		// This also catches "amount": 10.5 and "amount": "1000", because
		// neither decodes into an int64.
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "body_invalid", msg)
		return req, false
	}
	// Exactly one JSON value: reject trailing data like `{...}{...}`.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "body_invalid", msg)
		return req, false
	}
	return req, true
}

// handleGetPayment implements GET /v1/payments/{id}.
func (s *Server) handleGetPayment(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetPayment(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errTypeInvalidRequest, "payment_not_found", "no payment with that id")
	case err != nil:
		LoggerFrom(r.Context(), s.logger).Error("get payment", "error", err)
		writeError(w, http.StatusInternalServerError, errTypeAPI, "internal_error", "internal error")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}