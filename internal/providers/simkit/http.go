package simkit

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

// Problem codes every simulator answers with.
const (
	CodeInvalid        = "invalid_request"
	CodeKeyReused      = "idempotency_key_reused"
	CodeFailedFault    = "internal_error"
	CodeEventNotFound  = "event_not_found"
	CodeDeliveryFailed = "event_delivery_failed"
)

const (
	MaxKeyLength   = 255
	MaxRequestBody = 1 << 16
)

// ValidKey accepts 1 to MaxKeyLength printable ASCII characters.
func ValidKey(key string) bool {
	if key == "" || len(key) > MaxKeyLength {
		return false
	}
	for i := range len(key) {
		if key[i] < 0x20 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// IdempotencyKey reads and checks the request's key, answering 400 when it is not
// valid.
func IdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if !ValidKey(key) {
		Invalid(w, r, "Idempotency-Key must have 1 to 255 printable ASCII characters")
		return "", false
	}
	return key, true
}

// Decode reads one JSON value, refusing unknown fields and bodies over
// MaxRequestBody.
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func Invalid(w http.ResponseWriter, r *http.Request, detail string) {
	httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusBadRequest, CodeInvalid, detail))
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
