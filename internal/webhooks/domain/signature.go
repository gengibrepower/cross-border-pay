package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
)

// The same scheme the PIX simulator signs its events with: the HMAC-SHA256 of the Unix
// time, a dot and the body, so the timestamp cannot be swapped and an old capture
// cannot be replayed.
const (
	SignatureHeader    = "Webhook-Signature"
	SignatureTolerance = 5 * time.Minute
	// PreviousSecretGrace is how long after a rotation the previous secret still signs,
	// so a merchant can switch without losing webhooks, and a leaked secret that was
	// rotated away stops working soon after.
	PreviousSecretGrace = 24 * time.Hour
)

var (
	ErrBadSignature   = errors.New("webhooks: signature does not match")
	ErrStaleSignature = errors.New("webhooks: signature is too old or from the future")
)

// SigningVersions are the merchant's secrets a delivery at now is signed with: the
// current one, and the previous one within PreviousSecretGrace of the rotation.
func SigningVersions(e *merchants.Endpoint, now time.Time) []merchants.SecretVersion {
	if e.Previous != 0 && now.Sub(e.RotatedAt) < PreviousSecretGrace {
		return []merchants.SecretVersion{e.Version, e.Previous}
	}
	return []merchants.SecretVersion{e.Version}
}

// Sign returns the signature header for body sent at t, with one v1 per secret.
func Sign(secrets []string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	var b strings.Builder
	b.WriteString("t=" + ts)
	for _, s := range secrets {
		b.WriteString(",v1=" + hex.EncodeToString(mac(s, ts, body)))
	}
	return b.String()
}

func mac(secret, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// Verify is what a merchant runs on a webhook: any v1 may match its secret.
func Verify(secret, header string, body []byte, now time.Time) error {
	var ts string
	var sigs [][]byte
	for part := range strings.SplitSeq(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			if sig, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, sig)
			}
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return fmt.Errorf("%w: header %q", ErrBadSignature, header)
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return fmt.Errorf("%w: signed %v away from now", ErrStaleSignature, d)
	}
	want := mac(secret, ts, body)
	matched := false
	for _, sig := range sigs {
		matched = hmac.Equal(sig, want) || matched
	}
	if !matched {
		return ErrBadSignature
	}
	return nil
}
