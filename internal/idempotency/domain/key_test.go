package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func TestDecide(t *testing.T) {
	hash := domain.HashRequest("POST", "/v1/payments", []byte(`{"a":1}`))
	other := domain.HashRequest("POST", "/v1/payments", []byte(`{"a":2}`))
	processing := domain.Record{Hash: hash, Status: domain.Processing, LockedUntil: t0.Add(domain.Lease), CreatedAt: t0}
	completed := domain.Record{Hash: hash, Status: domain.Completed, CreatedAt: t0}

	tests := []struct {
		name   string
		record domain.Record
		hash   domain.RequestHash
		at     time.Time
		want   domain.Outcome
	}{
		{"same request, first finished", completed, hash, t0.Add(time.Minute), domain.Replay},
		{"different request, first finished", completed, other, t0.Add(time.Minute), domain.Reused},
		{"same request, lease running", processing, hash, t0.Add(domain.Lease - time.Microsecond), domain.InProgress},
		{"different request, lease running", processing, other, t0, domain.Reused},
		{"same request, lease over", processing, hash, t0.Add(domain.Lease), domain.Resume},
		{"completed key at the TTL", completed, other, t0.Add(domain.TTL), domain.Restart},
		{"processing key at the TTL", processing, hash, t0.Add(domain.TTL), domain.Restart},
		{"just before the TTL", completed, hash, t0.Add(domain.TTL - time.Microsecond), domain.Replay},
	}
	for _, tt := range tests {
		if got := domain.Decide(&tt.record, tt.hash, tt.at); got != tt.want {
			t.Errorf("%s: Decide = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestHashRequestCoversMethodPathAndBody(t *testing.T) {
	base := domain.HashRequest("POST", "/v1/payments", []byte("{}"))
	variants := map[string]domain.RequestHash{
		"method":                 domain.HashRequest("PUT", "/v1/payments", []byte("{}")),
		"path":                   domain.HashRequest("POST", "/v1/refunds", []byte("{}")),
		"body":                   domain.HashRequest("POST", "/v1/payments", []byte("{ }")),
		"path moved into body":   domain.HashRequest("POST", "/v1/payments\n{}", nil),
		"body moved into path":   domain.HashRequest("POST", "", []byte("/v1/payments\n{}")),
		"method moved into path": domain.HashRequest("", "POST\n/v1/payments", []byte("{}")),
	}
	for name, h := range variants {
		if h == base {
			t.Errorf("changing the %s does not change the hash", name)
		}
	}
	if domain.HashRequest("POST", "/v1/payments", []byte("{}")) != base {
		t.Error("the same request hashes differently")
	}
}

func TestNewRequestKeyLength(t *testing.T) {
	for _, n := range []int{1, 255} {
		if _, err := domain.NewRequest(domain.MerchantID{}, strings.Repeat("k", n), domain.RequestHash{}); err != nil {
			t.Errorf("key of %d characters: %v", n, err)
		}
	}
	for _, n := range []int{0, 256} {
		if _, err := domain.NewRequest(domain.MerchantID{}, strings.Repeat("k", n), domain.RequestHash{}); !errors.Is(err, domain.ErrInvalidKey) {
			t.Errorf("key of %d characters: error = %v, want %v", n, err, domain.ErrInvalidKey)
		}
	}
}
