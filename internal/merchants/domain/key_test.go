package domain_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
)

func TestGenerateKey(t *testing.T) {
	for _, mode := range []domain.Mode{domain.Test, domain.Live} {
		k, err := domain.GenerateKey(mode, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(k.Plaintext, "sk_"+string(mode)+"_"+k.Lookup+"_") {
			t.Errorf("Plaintext %q does not start with sk_%s_%s_", k.Plaintext, mode, k.Lookup)
		}
		if k.Hash != sha256.Sum256([]byte(k.Plaintext)) {
			t.Error("Hash is not the SHA-256 of the plaintext")
		}

		gotMode, lookup, err := domain.ParseKey(k.Plaintext)
		if err != nil || gotMode != mode || lookup != k.Lookup {
			t.Errorf("ParseKey(%q) = %q, %q, %v; want %q, %q", k.Plaintext, gotMode, lookup, err, mode, k.Lookup)
		}
		if !domain.Matches(k.Plaintext, k.Hash[:]) {
			t.Error("Matches(own key) = false")
		}
	}
}

func TestGenerateKeyIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		k, err := domain.GenerateKey(domain.Test, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if seen[k.Lookup] {
			t.Fatalf("lookup %q repeated", k.Lookup)
		}
		seen[k.Lookup] = true
	}
}

func TestGenerateKeyErrors(t *testing.T) {
	if _, err := domain.GenerateKey("prod", rand.Reader); !errors.Is(err, domain.ErrUnknownMode) {
		t.Errorf("unknown mode: error = %v, want %v", err, domain.ErrUnknownMode)
	}
	short := bytes.NewReader(make([]byte, 10))
	if _, err := domain.GenerateKey(domain.Test, short); err == nil {
		t.Error("GenerateKey accepted a random source that ran dry")
	}
	if _, err := domain.GenerateKey(domain.Test, iotest.ErrReader(errors.New("no entropy"))); err == nil {
		t.Error("GenerateKey ignored a failing random source")
	}
}

func TestParseKeyRejects(t *testing.T) {
	k, err := domain.GenerateKey(domain.Test, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(k.Plaintext, "_")
	tests := map[string]string{
		"empty":           "",
		"no prefix":       strings.TrimPrefix(k.Plaintext, "sk_"),
		"public prefix":   "pk_" + strings.TrimPrefix(k.Plaintext, "sk_"),
		"unknown mode":    strings.Replace(k.Plaintext, "_test_", "_prod_", 1),
		"short lookup":    "sk_test_" + parts[2][1:] + "_" + parts[3],
		"short secret":    "sk_test_" + parts[2] + "_" + parts[3][1:],
		"upper case":      strings.ToUpper(k.Plaintext),
		"invalid base32":  "sk_test_" + parts[2] + "_" + strings.Repeat("1", len(parts[3])),
		"extra separator": k.Plaintext + "_x",
	}
	for name, key := range tests {
		if _, _, err := domain.ParseKey(key); !errors.Is(err, domain.ErrMalformedKey) {
			t.Errorf("%s: ParseKey(%q) error = %v, want %v", name, key, err, domain.ErrMalformedKey)
		}
	}
}

func TestMatchesRejectsOtherKeys(t *testing.T) {
	a, err := domain.GenerateKey(domain.Test, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := domain.GenerateKey(domain.Test, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if domain.Matches(b.Plaintext, a.Hash[:]) {
		t.Error("Matches(other key) = true")
	}
	if domain.Matches(a.Plaintext, a.Hash[:16]) {
		t.Error("Matches(truncated hash) = true")
	}
}
