package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrSecretVersionConflict is a rotation from a version that is neither the current
	// one nor the one just before it: the caller's view of the secret is stale.
	ErrSecretVersionConflict = errors.New("merchants: webhook secret version conflict")
	ErrWeakSigningKey        = errors.New("merchants: webhook signing key must have at least 32 bytes")
)

// MinSigningKeyBytes keeps the derived secrets as strong as their HMAC.
const MinSigningKeyBytes = 32

// SecretVersion numbers a merchant's webhook secrets; rotating moves to the next one.
type SecretVersion int

// Endpoint is where a merchant receives webhooks. It holds no secret, only which
// version of it is current: see DeriveSecret.
type Endpoint struct {
	Merchant  MerchantID
	URL       string
	Version   SecretVersion
	Previous  SecretVersion // 0 until the first rotation
	RotatedAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Change is a PUT of the endpoint: the URL, and RotateFrom when the merchant asks for
// a new secret, naming the version it is replacing.
type Change struct {
	URL        string
	RotateFrom SecretVersion // 0: keep the secret
}

// Apply returns the endpoint after the change, from current (nil for none), and
// whether its secret is shown in the answer: on the first registration and on a
// rotation. Rotating is conditional on the version named, so a repeated request
// (a client retrying after a timeout) finds the secret already rotated from that
// version, does not rotate again and shows it again.
func Apply(current *Endpoint, merchant MerchantID, c Change, now time.Time) (Endpoint, bool, error) {
	if current == nil {
		if c.RotateFrom != 0 {
			return Endpoint{}, false, fmt.Errorf("%w: no secret to rotate from %d yet", ErrSecretVersionConflict, c.RotateFrom)
		}
		return Endpoint{Merchant: merchant, URL: c.URL, Version: 1, CreatedAt: now, UpdatedAt: now}, true, nil
	}
	next := *current
	next.URL, next.UpdatedAt = c.URL, now
	switch c.RotateFrom {
	case 0:
		return next, false, nil
	case current.Version:
		next.Previous, next.Version, next.RotatedAt = current.Version, current.Version+1, now
		return next, true, nil
	case current.Previous:
		return next, true, nil
	default:
		return Endpoint{}, false, fmt.Errorf("%w: rotating from %d, current is %d", ErrSecretVersionConflict, c.RotateFrom, current.Version)
	}
}

// DeriveSecret computes a merchant's webhook secret. Nothing stores it: the key lives
// outside the database, so neither a dump nor a log of it can sign a webhook.
func DeriveSecret(key []byte, merchant MerchantID, v SecretVersion) (string, error) {
	if len(key) < MinSigningKeyBytes {
		return "", ErrWeakSigningKey
	}
	if v < 1 {
		return "", fmt.Errorf("merchants: secret version %d", v)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("webhook-secret\x00"))
	mac.Write(merchant[:])
	mac.Write(binary.BigEndian.AppendUint64(nil, uint64(v)))
	return "whsec_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
