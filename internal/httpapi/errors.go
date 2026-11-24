package httpapi

import "net/http"

// Error types. "type" is the broad category a client branches on; "code" is
// the specific reason. This mirrors Stripe's error shape:
//
//	{"error": {"type": "invalid_request_error", "code": "amount_invalid", "message": "..."}}
const (
	errTypeInvalidRequest = "invalid_request_error" // client sent something wrong; don't retry as-is
	errTypeIdempotency    = "idempotency_error"     // key reused with a different body
	errTypeAPI            = "api_error"             // our fault; safe to retry with the same key
)

type apiError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSON(w, status, map[string]apiError{
		"error": {Type: errType, Code: code, Message: message},
	})
}