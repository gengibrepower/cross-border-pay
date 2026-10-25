package adapters_test

import (
	"context"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
)

func (e *env) keys() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), "SELECT count(*) FROM idempotency_keys").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// Decision 14: the cleanup deletes every key past the TTL, across several batches,
// and nothing younger.
func TestCleanupDeletesOnlyExpiredKeys(t *testing.T) {
	e := newEnv(t)
	m := e.merchant()
	const old, batch = 7, 3
	for i := range old {
		e.hold(request(t, m, "old-"+string(rune('a'+i)), "body"))
	}
	e.clock.Advance(time.Hour)
	e.hold(request(t, m, "young", "body"))

	e.clock.Advance(domain.TTL - time.Hour)
	deleted, err := e.svc.Cleanup(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != old || e.keys() != 1 {
		t.Errorf("deleted %d and %d keys left, want %d deleted and only the young one left", deleted, e.keys(), old)
	}
}

// SKIP LOCKED: a key another transaction has locked is passed over, not waited for.
func TestCleanupSkipsLockedKeys(t *testing.T) {
	e := newEnv(t)
	m := e.merchant()
	e.hold(request(t, m, "locked", "body"))
	e.hold(request(t, m, "free", "body"))
	e.clock.Advance(domain.TTL)

	tx, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := tx.Exec(t.Context(), "SELECT 1 FROM idempotency_keys WHERE key = 'locked' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	deleted, err := e.svc.Cleanup(ctx, 10)
	if err != nil {
		t.Fatalf("Cleanup with a locked key: %v; it must not wait for the lock", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d, want only the free key", deleted)
	}

	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if deleted, err := e.svc.Cleanup(t.Context(), 10); err != nil || deleted != 1 {
		t.Errorf("after the lock: Cleanup = %d, %v; want the other key deleted", deleted, err)
	}
}
