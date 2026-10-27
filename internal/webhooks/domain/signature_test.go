package domain

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
)

var t0 = time.Date(2026, 11, 3, 14, 21, 7, 0, time.UTC)

const body = `{"id":"evt_1","type":"payment.paid"}`

func TestVerifyAcceptsWhatSignSigned(t *testing.T) {
	header := Sign([]string{"whsec_a"}, t0, []byte(body))
	if err := Verify("whsec_a", header, []byte(body), t0); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
}

func TestVerifyRefusesForgeries(t *testing.T) {
	header := Sign([]string{"whsec_a"}, t0, []byte(body))
	ts, sig, _ := strings.Cut(header, ",")
	tests := map[string]struct {
		secret, header, body string
	}{
		"another body":      {"whsec_a", header, body + " "},
		"another secret":    {"whsec_b", header, body},
		"timestamp swapped": {"whsec_a", "t=" + strconv.FormatInt(t0.Unix()+1, 10) + "," + sig, body},
		"no signature":      {"whsec_a", ts, body},
		"no timestamp":      {"whsec_a", sig, body},
		"empty":             {"whsec_a", "", body},
		"not hex":           {"whsec_a", ts + ",v1=zz", body},
	}
	for name, tc := range tests {
		if err := Verify(tc.secret, tc.header, []byte(tc.body), t0); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: Verify() = %v, want %v", name, err, ErrBadSignature)
		}
	}
}

func TestVerifyRefusesOldAndFutureSignatures(t *testing.T) {
	for _, d := range []time.Duration{SignatureTolerance + time.Second, -SignatureTolerance - time.Second} {
		header := Sign([]string{"whsec_a"}, t0.Add(-d), []byte(body))
		if err := Verify("whsec_a", header, []byte(body), t0); !errors.Is(err, ErrStaleSignature) {
			t.Errorf("signed %v ago: Verify() = %v, want %v", d, err, ErrStaleSignature)
		}
	}
	header := Sign([]string{"whsec_a"}, t0.Add(-SignatureTolerance), []byte(body))
	if err := Verify("whsec_a", header, []byte(body), t0); err != nil {
		t.Errorf("signed at the tolerance: Verify() = %v", err)
	}
}

// While a merchant switches secrets, each of the two verifies the same delivery.
func TestSignWithTwoSecretsVerifiesWithEither(t *testing.T) {
	header := Sign([]string{"whsec_new", "whsec_old"}, t0, []byte(body))
	for _, s := range []string{"whsec_new", "whsec_old"} {
		if err := Verify(s, header, []byte(body), t0); err != nil {
			t.Errorf("Verify() with %s = %v", s, err)
		}
	}
	if err := Verify("whsec_other", header, []byte(body), t0); !errors.Is(err, ErrBadSignature) {
		t.Errorf("Verify() with another secret = %v, want %v", err, ErrBadSignature)
	}
}

func TestSigningVersions(t *testing.T) {
	never := merchants.Endpoint{Version: 1}
	rotated := merchants.Endpoint{Version: 3, Previous: 2, RotatedAt: t0}
	tests := []struct {
		name string
		e    merchants.Endpoint
		now  time.Time
		want []merchants.SecretVersion
	}{
		{"never rotated", never, t0, []merchants.SecretVersion{1}},
		{"just rotated", rotated, t0, []merchants.SecretVersion{3, 2}},
		{"within the grace", rotated, t0.Add(PreviousSecretGrace - time.Microsecond), []merchants.SecretVersion{3, 2}},
		{"after the grace", rotated, t0.Add(PreviousSecretGrace), []merchants.SecretVersion{3}},
	}
	for _, tc := range tests {
		got := SigningVersions(&tc.e, tc.now)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: SigningVersions() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
