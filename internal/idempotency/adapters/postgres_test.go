package adapters_test

import (
	"crypto/rand"
	"errors"
	"maps"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	"github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	clock *clock.Fake
	svc   *app.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.NewPool(t)
	c := clock.NewFake(t0)
	return &env{t: t, pool: pool, clock: c, svc: app.NewService(adapters.NewPostgres(pool), c, rand.Reader)}
}

func (e *env) merchant() domain.MerchantID {
	e.t.Helper()
	var id [16]byte
	if err := e.pool.QueryRow(e.t.Context(), "INSERT INTO merchants (name) VALUES ('m') RETURNING id").Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func request(t *testing.T, merchant domain.MerchantID, key, body string) *domain.Request {
	t.Helper()
	req, err := domain.NewRequest(merchant, key, domain.HashRequest("POST", "/v1/payments", []byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	return &req
}

func (e *env) hold(req *domain.Request) *domain.Held {
	e.t.Helper()
	a, err := e.svc.Begin(e.t.Context(), req)
	if err != nil || a.Held == nil {
		e.t.Fatalf("Begin = %+v, %v; want a held key", a, err)
	}
	return a.Held
}

func (e *env) replay(req *domain.Request) *domain.Response {
	e.t.Helper()
	a, err := e.svc.Begin(e.t.Context(), req)
	if err != nil || a.Replay == nil {
		e.t.Fatalf("Begin = %+v, %v; want a replay", a, err)
	}
	return a.Replay
}

func (e *env) beginErr(req *domain.Request) error {
	e.t.Helper()
	a, err := e.svc.Begin(e.t.Context(), req)
	if err == nil {
		e.t.Fatalf("Begin = %+v, want an error", a)
	}
	return err
}

func sameResponse(a, b *domain.Response) bool {
	return a.Status == b.Status && maps.Equal(a.Headers, b.Headers) && slices.Equal(a.Body, b.Body)
}

var created = &domain.Response{
	Status:  201,
	Headers: map[string]string{"Content-Type": "application/json", "Location": "/v1/payments/pay_1"},
	Body:    []byte(`{"id":"pay_1"}`),
}

// Section 4.2, case 1.
func TestSameRequestAfterCompletionReplaysTheResponse(t *testing.T) {
	e := newEnv(t)
	req := request(t, e.merchant(), "k", "body")
	if err := e.svc.Complete(t.Context(), e.hold(req), created); err != nil {
		t.Fatal(err)
	}
	if got := e.replay(req); !sameResponse(got, created) {
		t.Errorf("replayed %+v, want %+v", got, created)
	}
}

// Case 2, both while the first request runs and after it finished.
func TestSameKeyDifferentRequestIsReused(t *testing.T) {
	e := newEnv(t)
	m := e.merchant()
	held := e.hold(request(t, m, "k", "body"))
	if err := e.beginErr(request(t, m, "k", "other body")); !errors.Is(err, app.ErrKeyReused) {
		t.Errorf("while processing: error = %v, want %v", err, app.ErrKeyReused)
	}
	if err := e.svc.Complete(t.Context(), held, created); err != nil {
		t.Fatal(err)
	}
	if err := e.beginErr(request(t, m, "k", "other body")); !errors.Is(err, app.ErrKeyReused) {
		t.Errorf("after completion: error = %v, want %v", err, app.ErrKeyReused)
	}
}

// Case 3 and the F4 done criterion: of many concurrent requests with one key, exactly
// one runs and every other gets the immediate 409 of decision 15.
func TestConcurrentRequestsWithOneKey(t *testing.T) {
	e := newEnv(t)
	req := request(t, e.merchant(), "k", "body")
	const n = 20

	start := make(chan struct{})
	results := make([]error, n)
	held := make([]*domain.Held, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			a, err := e.svc.Begin(t.Context(), req)
			held[i], results[i] = a.Held, err
		})
	}
	close(start)
	wg.Wait()

	var winners, inProgress int
	for i, err := range results {
		switch {
		case err == nil && held[i] != nil:
			winners++
		case errors.Is(err, app.ErrInProgress):
			inProgress++
		default:
			t.Errorf("request %d: Begin error = %v", i, err)
		}
	}
	if winners != 1 || inProgress != n-1 {
		t.Errorf("%d held the key and %d got ErrInProgress, want 1 and %d", winners, inProgress, n-1)
	}
}

// Case 4: a request that died holds the key until its lease ends; then a retry resumes
// from where it got, and the dead request can no longer touch the key.
func TestRetryResumesAfterTheLeaseEnds(t *testing.T) {
	e := newEnv(t)
	req := request(t, e.merchant(), "k", "body")
	dead := e.hold(req)
	resource := [16]byte{7}
	if err := adapters.SetRecoveryPoint(t.Context(), e.pool, dead, "payment_created", resource); err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(domain.Lease - time.Microsecond)
	if err := e.beginErr(req); !errors.Is(err, app.ErrInProgress) {
		t.Fatalf("before the lease ends: error = %v, want %v", err, app.ErrInProgress)
	}

	e.clock.Advance(time.Microsecond)
	resumed := e.hold(req)
	if resumed.RecoveryPoint != "payment_created" || resumed.ResourceID != resource {
		t.Errorf("resumed at %q with resource %x, want payment_created and %x", resumed.RecoveryPoint, resumed.ResourceID, resource)
	}

	if err := e.svc.Complete(t.Context(), dead, created); !errors.Is(err, app.ErrLeaseLost) {
		t.Errorf("dead holder Complete error = %v, want %v", err, app.ErrLeaseLost)
	}
	if err := adapters.SetRecoveryPoint(t.Context(), e.pool, dead, "other", resource); !errors.Is(err, app.ErrLeaseLost) {
		t.Errorf("dead holder SetRecoveryPoint error = %v, want %v", err, app.ErrLeaseLost)
	}
	if _, err := e.svc.Fail(t.Context(), dead); !errors.Is(err, app.ErrLeaseLost) {
		t.Errorf("dead holder Fail error = %v, want %v", err, app.ErrLeaseLost)
	}
	if err := e.svc.Complete(t.Context(), resumed, created); err != nil {
		t.Errorf("resumed holder Complete: %v", err)
	}
}

// Case 5: a 4xx is an answer like any other, kept and replayed.
func TestClientErrorIsKeptAndReplayed(t *testing.T) {
	e := newEnv(t)
	req := request(t, e.merchant(), "k", "body")
	rejected := &domain.Response{Status: 422, Headers: map[string]string{"Content-Type": "application/problem+json"}, Body: []byte(`{"code":"invalid_amount"}`)}
	if err := e.svc.Complete(t.Context(), e.hold(req), rejected); err != nil {
		t.Fatal(err)
	}
	if got := e.replay(req); !sameResponse(got, rejected) {
		t.Errorf("replayed %+v, want %+v", got, rejected)
	}
}

// Case 6: a 5xx before anything was done releases the key; the retry starts fresh.
func TestServerErrorBeforeAnyWorkReleasesTheKey(t *testing.T) {
	e := newEnv(t)
	req := request(t, e.merchant(), "k", "body")
	released, err := e.svc.Fail(t.Context(), e.hold(req))
	if err != nil || !released {
		t.Fatalf("Fail = %v, %v; want released", released, err)
	}
	if retry := e.hold(req); retry.RecoveryPoint != domain.Started || retry.ResourceID != [16]byte{} {
		t.Errorf("retry held %+v, want a fresh key", retry)
	}
}

// Case 7: a 5xx after work was recorded (in F5, after the provider call) keeps the
// key, so a retry cannot start over and create a second attempt. The lease ends at
// once, and the retry resumes from the recovery point without waiting for it.
func TestServerErrorAfterWorkKeepsTheKey(t *testing.T) {
	e := newEnv(t)
	req := request(t, e.merchant(), "k", "body")
	held := e.hold(req)
	if err := adapters.SetRecoveryPoint(t.Context(), e.pool, held, "attempt_requested", [16]byte{9}); err != nil {
		t.Fatal(err)
	}
	released, err := e.svc.Fail(t.Context(), held)
	if err != nil || released {
		t.Fatalf("Fail = %v, %v; want the key kept", released, err)
	}
	if retry := e.hold(req); retry.RecoveryPoint != "attempt_requested" || retry.ResourceID != [16]byte{9} {
		t.Errorf("retry held %+v, want it resumed at attempt_requested", retry)
	}
}

// Case 8.
func TestMerchantsKeysAreIndependent(t *testing.T) {
	e := newEnv(t)
	a, b := e.merchant(), e.merchant()
	e.hold(request(t, a, "k", "body"))
	e.hold(request(t, b, "k", "other body"))
}

// Decision 14 and section 4.6: a key past its TTL is treated as new even before the
// cleanup job deletes it.
func TestKeyPastTheTTLStartsOver(t *testing.T) {
	e := newEnv(t)
	m := e.merchant()
	if err := e.svc.Complete(t.Context(), e.hold(request(t, m, "k", "body")), created); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(domain.TTL)
	if fresh := e.hold(request(t, m, "k", "a different body")); fresh.RecoveryPoint != domain.Started {
		t.Errorf("held %+v, want a fresh key", fresh)
	}
}

// The recovery point is written on the caller's transaction: if the work rolls back,
// so does the recovery point, and a resumed request does not skip work never done.
func TestRecoveryPointRollsBackWithTheCallersWork(t *testing.T) {
	e := newEnv(t)
	req := request(t, e.merchant(), "k", "body")
	held := e.hold(req)

	err := pgx.BeginFunc(t.Context(), e.pool, func(tx pgx.Tx) error {
		if err := adapters.SetRecoveryPoint(t.Context(), tx, held, "payment_created", [16]byte{1}); err != nil {
			return err
		}
		return errors.New("the payment insert failed")
	})
	if err == nil {
		t.Fatal("transaction committed, want it rolled back")
	}

	e.clock.Advance(domain.Lease)
	if resumed := e.hold(req); resumed.RecoveryPoint != domain.Started {
		t.Errorf("resumed at %q, want %q: the rolled-back recovery point must not survive", resumed.RecoveryPoint, domain.Started)
	}
}
