package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

var ErrInvalidKey = errors.New("idempotency: key must have 1 to 255 characters")

const (
	// TTL is decision 14: after it, a key is as good as new.
	TTL = 24 * time.Hour
	// Lease is decision 52: how long a request holds its key before a retry may resume it.
	Lease = 30 * time.Second
)

// MerchantID is this module's name for the merchant a key belongs to; keys of
// different merchants never meet.
type MerchantID [16]byte

type RequestHash [32]byte

// HashRequest fingerprints the request as sent. A retry resends the same bytes, so the
// raw body is compared rather than its JSON meaning.
func HashRequest(method, path string, body []byte) RequestHash {
	h := sha256.New()
	// Neither method nor path can contain a newline, so the fields cannot run together.
	h.Write([]byte(method + "\n" + path + "\n"))
	h.Write(body)
	return RequestHash(h.Sum(nil))
}

type Request struct {
	Merchant MerchantID
	Key      string
	Hash     RequestHash
}

func NewRequest(merchant MerchantID, key string, hash RequestHash) (Request, error) {
	if len(key) < 1 || len(key) > 255 {
		return Request{}, fmt.Errorf("%w: got %d", ErrInvalidKey, len(key))
	}
	return Request{Merchant: merchant, Key: key, Hash: hash}, nil
}

type Status string

const (
	Processing Status = "processing"
	Completed  Status = "completed"
)

// RecoveryPoint is how far a request got. Started is the only one this module knows;
// each endpoint names the phases after it, and a resumed request continues from there.
type RecoveryPoint string

const Started RecoveryPoint = "started"

// Response is what a completed request answered, replayed as is on a retry.
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Record is a stored key as Decide needs it.
type Record struct {
	Hash        RequestHash
	Status      Status
	LockedUntil time.Time
	CreatedAt   time.Time
}

type Outcome int

const (
	// Restart: the key is older than TTL, so the request starts over as if it were new.
	Restart Outcome = iota
	// Resume: the previous holder's lease ran out; continue from its recovery point.
	Resume
	Replay
	// InProgress is the immediate 409 of decision 15.
	InProgress
	// Reused is the same key with a different request: 422.
	Reused
)

// Decide applies section 4.2 to a key that already exists. The body is compared
// before the lock, so a reused key is reported even while the first request runs.
func Decide(r *Record, hash RequestHash, now time.Time) Outcome {
	switch {
	case !now.Before(r.CreatedAt.Add(TTL)):
		return Restart
	case r.Hash != hash:
		return Reused
	case r.Status == Completed:
		return Replay
	case now.Before(r.LockedUntil):
		return InProgress
	default:
		return Resume
	}
}

// LeaseToken proves who holds a key, so a request whose lease ran out cannot complete
// or release a key that a retry has resumed since.
type LeaseToken [16]byte

// Held is a key the caller may run its request under.
type Held struct {
	Merchant      MerchantID
	Key           string
	Token         LeaseToken
	RecoveryPoint RecoveryPoint
	// ResourceID is what an earlier holder created, if it got that far; zero otherwise.
	ResourceID [16]byte
}
