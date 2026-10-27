package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
)

var (
	t0      = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	gameID  = domain.MerchantID{1}
	signKey = []byte(strings.Repeat("k", domain.MinSigningKeyBytes))
)

func TestFirstRegistrationShowsVersionOne(t *testing.T) {
	e, shown, err := domain.Apply(nil, gameID, domain.Change{URL: "https://a.example"}, t0)
	if err != nil || !shown || e.Version != 1 || e.Previous != 0 || e.URL != "https://a.example" || e.Merchant != gameID {
		t.Errorf("Apply() = %+v, %v, %v; want version 1, shown", e, shown, err)
	}
	if _, _, err := domain.Apply(nil, gameID, domain.Change{URL: "https://a.example", RotateFrom: 1}, t0); !errors.Is(err, domain.ErrSecretVersionConflict) {
		t.Errorf("rotating before registering: error = %v, want %v", err, domain.ErrSecretVersionConflict)
	}
}

func TestRotation(t *testing.T) {
	v3 := domain.Endpoint{Merchant: gameID, URL: "https://a.example", Version: 3, Previous: 2, RotatedAt: t0, CreatedAt: t0, UpdatedAt: t0}
	later := t0.Add(time.Hour)
	for _, tc := range []struct {
		name          string
		from          domain.SecretVersion
		version, prev domain.SecretVersion
		shown         bool
		err           error
	}{
		{"new URL only", 0, 3, 2, false, nil},
		{"from the current version", 3, 4, 3, true, nil},
		{"the same request again", 2, 3, 2, true, nil},
		{"from an older version", 1, 0, 0, false, domain.ErrSecretVersionConflict},
		{"from a version not issued", 4, 0, 0, false, domain.ErrSecretVersionConflict},
	} {
		e, shown, err := domain.Apply(&v3, gameID, domain.Change{URL: "https://b.example", RotateFrom: tc.from}, later)
		if !errors.Is(err, tc.err) || shown != tc.shown {
			t.Errorf("%s: shown %v, error %v; want %v, %v", tc.name, shown, err, tc.shown, tc.err)
			continue
		}
		if err == nil && (e.Version != tc.version || e.Previous != tc.prev || e.URL != "https://b.example" || !e.UpdatedAt.Equal(later)) {
			t.Errorf("%s: %+v, want version %d after %d at the new URL", tc.name, e, tc.version, tc.prev)
		}
	}
	if e, _, _ := domain.Apply(&v3, gameID, domain.Change{URL: "https://a.example", RotateFrom: 3}, later); !e.RotatedAt.Equal(later) {
		t.Errorf("rotated at %s, want %s", e.RotatedAt, later)
	}
}

func TestDeriveSecret(t *testing.T) {
	s1, err := domain.DeriveSecret(signKey, gameID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s1, "whsec_") || len(s1) != len("whsec_")+43 {
		t.Errorf("secret %q, want whsec_ and 32 bytes in base64url", s1)
	}
	again, _ := domain.DeriveSecret(signKey, gameID, 1)
	s2, _ := domain.DeriveSecret(signKey, gameID, 2)
	other, _ := domain.DeriveSecret(signKey, domain.MerchantID{2}, 1)
	otherKey, _ := domain.DeriveSecret([]byte(strings.Repeat("j", domain.MinSigningKeyBytes)), gameID, 1)
	if again != s1 {
		t.Error("the same merchant and version derived two secrets")
	}
	if s2 == s1 || other == s1 || otherKey == s1 {
		t.Error("another version, merchant or key derived the same secret")
	}
	if _, err := domain.DeriveSecret(signKey[:domain.MinSigningKeyBytes-1], gameID, 1); !errors.Is(err, domain.ErrWeakSigningKey) {
		t.Errorf("short key: error = %v, want %v", err, domain.ErrWeakSigningKey)
	}
}
