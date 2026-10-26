package pixsim_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

var secret = []byte("s3cret")

const paidBody = `{"id":"evt_1","type":"charge.paid","created_at":"2026-11-01T12:00:00Z","charge":{"id":"chg_1","idempotency_key":"att_1","status":"paid","amount":{"value":10150,"currency":"BRL"},"expires_at":"2026-11-01T12:30:00Z","created_at":"2026-11-01T12:00:00Z"}}`

func TestVerifyEventAcceptsWhatSignSigned(t *testing.T) {
	e, err := pixsim.VerifyEvent(secret, pixsim.Sign(secret, t0, []byte(paidBody)), []byte(paidBody), t0)
	if err != nil || e.ID != "evt_1" || e.Type != pixsim.ChargePaid || e.Charge.ID != "chg_1" || e.Charge.Status != pixsim.Paid {
		t.Fatalf("VerifyEvent() = %+v, %v", e, err)
	}
}

func TestVerifyEventRefusesForgeries(t *testing.T) {
	signed := pixsim.Sign(secret, t0, []byte(paidBody))
	ts, sig, _ := strings.Cut(signed, ",")
	later, _, _ := strings.Cut(pixsim.Sign(secret, t0.Add(time.Second), []byte(paidBody)), ",")
	for name, tc := range map[string]struct {
		header string
		body   string
	}{
		"tampered body":       {signed, strings.Replace(paidBody, "10150", "99999", 1)},
		"wrong secret":        {pixsim.Sign([]byte("other"), t0, []byte(paidBody)), paidBody},
		"time changed":        {later + "," + sig, paidBody},
		"no header":           {"", paidBody},
		"no signature":        {ts, paidBody},
		"no time":             {sig, paidBody},
		"signature not hex":   {ts + ",v1=zz", paidBody},
		"unknown scheme only": {ts + ",v0=" + sig[3:], paidBody},
	} {
		if _, err := pixsim.VerifyEvent(secret, tc.header, []byte(tc.body), t0); !errors.Is(err, pixsim.ErrBadSignature) {
			t.Errorf("%s: error = %v, want %v", name, err, pixsim.ErrBadSignature)
		}
	}
}

// The timestamp is signed with the body, so an old capture cannot be replayed with a
// fresh time, and an old time is refused.
func TestVerifyEventRefusesOldAndFutureSignatures(t *testing.T) {
	body := []byte(paidBody)
	for _, tc := range []struct {
		signedAt time.Duration
		err      error
	}{
		{-pixsim.SignatureTolerance, nil},
		{pixsim.SignatureTolerance, nil},
		{-pixsim.SignatureTolerance - time.Second, pixsim.ErrStaleSignature},
		{pixsim.SignatureTolerance + time.Second, pixsim.ErrStaleSignature},
	} {
		_, err := pixsim.VerifyEvent(secret, pixsim.Sign(secret, t0.Add(tc.signedAt), body), body, t0)
		if !errors.Is(err, tc.err) {
			t.Errorf("signed %v from now: error = %v, want %v", tc.signedAt, err, tc.err)
		}
	}
}

// While a secret rotates, the sender signs with both; either one is enough.
func TestVerifyEventAcceptsAnyMatchingSignature(t *testing.T) {
	body := []byte(paidBody)
	_, old, _ := strings.Cut(pixsim.Sign([]byte("old"), t0, body), ",")
	header := pixsim.Sign(secret, t0, body) + "," + old
	if _, err := pixsim.VerifyEvent(secret, header, body, t0); err != nil {
		t.Errorf("new secret: %v", err)
	}
	if _, err := pixsim.VerifyEvent([]byte("old"), header, body, t0); err != nil {
		t.Errorf("old secret: %v", err)
	}
}

// A signed body is trusted to come from the provider, not to be well formed.
func TestVerifyEventRefusesMalformedEvents(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":      `{"id":`,
		"unknown field": `{"id":"evt_1","type":"charge.paid","extra":1}`,
		"no id":         `{"type":"charge.paid"}`,
		"no type":       `{"id":"evt_1"}`,
		"float amount":  strings.Replace(paidBody, `"value":10150`, `"value":101.5`, 1),
	} {
		_, err := pixsim.VerifyEvent(secret, pixsim.Sign(secret, t0, []byte(body)), []byte(body), t0)
		if !errors.Is(err, pixsim.ErrBadEvent) {
			t.Errorf("%s: error = %v, want %v", name, err, pixsim.ErrBadEvent)
		}
	}
}
