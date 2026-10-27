package adapters_test

import (
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
)

var b0 = time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)

// breakerEnv is one merchant with an endpoint registered at b0.
func breakerEnv(t *testing.T) (*adapters.Postgres, *app.Endpoints, domain.MerchantID) {
	t.Helper()
	store := adapters.NewPostgres(pgtest.NewPool(t))
	merchant, _, err := app.NewService(store, rand.Reader).CreateMerchant(t.Context(), "GameStore", domain.Test)
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := app.NewEndpoints(store, ssrf.Policy{}, []byte(strings.Repeat("k", domain.MinSigningKeyBytes)), clock.NewFake(b0))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := endpoints.Put(t.Context(), merchant, domain.Change{URL: "https://gamestore.example/hooks"}); err != nil {
		t.Fatal(err)
	}
	return store, endpoints, merchant
}

func TestOnlyFailuresFor72HoursDisableTheEndpoint(t *testing.T) {
	store, endpoints, merchant := breakerEnv(t)
	ctx := t.Context()
	fail := func(at time.Duration) bool {
		t.Helper()
		disabled, err := store.EndpointFailed(ctx, merchant, "answered 500", b0.Add(at))
		if err != nil {
			t.Fatal(err)
		}
		return disabled
	}

	if fail(0) || fail(time.Hour) || fail(domain.DisableAfter-time.Microsecond) {
		t.Fatal("disabled before 72 h of failures")
	}
	e, err := endpoints.Get(ctx, merchant)
	if err != nil || e.Disabled() || !e.FailingSince.Equal(b0) {
		t.Fatalf("endpoint = %+v, %v; want enabled, failing since the first failure", e, err)
	}
	if !fail(domain.DisableAfter) {
		t.Fatal("not disabled at 72 h of failures")
	}
	e, err = endpoints.Get(ctx, merchant)
	if err != nil || !e.DisabledAt.Equal(b0.Add(domain.DisableAfter)) || e.DisabledReason != "only failures for 72 h; last: answered 500" {
		t.Fatalf("endpoint = %+v, %v; want disabled at 72 h with the last reason", e, err)
	}
	if fail(domain.DisableAfter + time.Hour) {
		t.Error("a disabled endpoint was disabled again")
	}
	if err := store.EndpointSucceeded(ctx, merchant); err != nil {
		t.Fatal(err)
	}
	if e, _ := endpoints.Get(ctx, merchant); !e.Disabled() {
		t.Error("a success turned a disabled endpoint back on; only the merchant does")
	}

	if _, _, err := endpoints.Put(ctx, merchant, domain.Change{URL: "https://gamestore.example/hooks"}); err != nil {
		t.Fatal(err)
	}
	if e, _ := endpoints.Get(ctx, merchant); e.Disabled() || !e.FailingSince.IsZero() {
		t.Errorf("after a PUT: %+v, want enabled with no run of failures", e)
	}
}

func TestASuccessEndsTheRunOfFailures(t *testing.T) {
	store, endpoints, merchant := breakerEnv(t)
	ctx := t.Context()
	if _, err := store.EndpointFailed(ctx, merchant, "answered 500", b0); err != nil {
		t.Fatal(err)
	}
	if err := store.EndpointSucceeded(ctx, merchant); err != nil {
		t.Fatal(err)
	}
	later := b0.Add(time.Hour)
	if _, err := store.EndpointFailed(ctx, merchant, "answered 500", later); err != nil {
		t.Fatal(err)
	}
	if disabled, _ := store.EndpointFailed(ctx, merchant, "answered 500", b0.Add(domain.DisableAfter)); disabled {
		t.Error("disabled 72 h after a failure that a success followed")
	}
	if e, _ := endpoints.Get(ctx, merchant); !e.FailingSince.Equal(later) {
		t.Errorf("failing since %v, want the first failure after the success, %v", e.FailingSince, later)
	}
}

// Every post of a round to a dead merchant fails at once: one of them disables the
// endpoint, and only one is told it did, so the merchant is warned once.
func TestConcurrentFailuresDisableOnce(t *testing.T) {
	const n = 20
	store, _, merchant := breakerEnv(t)
	if _, err := store.EndpointFailed(t.Context(), merchant, "answered 500", b0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, n)
	for range n {
		wg.Go(func() {
			disabled, err := store.EndpointFailed(t.Context(), merchant, "answered 500", b0.Add(domain.DisableAfter))
			if err != nil {
				t.Error(err)
			}
			results <- disabled
		})
	}
	wg.Wait()
	close(results)
	disabled := 0
	for d := range results {
		if d {
			disabled++
		}
	}
	if disabled != 1 {
		t.Errorf("%d of %d concurrent failures disabled the endpoint, want 1", disabled, n)
	}
}

func TestBreakerLeavesAMerchantWithoutEndpointAlone(t *testing.T) {
	store := adapters.NewPostgres(pgtest.NewPool(t))
	if disabled, err := store.EndpointFailed(t.Context(), domain.MerchantID{9}, "x", b0); disabled || err != nil {
		t.Errorf("EndpointFailed() = %v, %v, want false and no error", disabled, err)
	}
	if err := store.EndpointSucceeded(t.Context(), domain.MerchantID{9}); err != nil {
		t.Errorf("EndpointSucceeded() = %v", err)
	}
}
