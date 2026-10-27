package adapters_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	eventsadapters "github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	merchantsapp "github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
	"github.com/gengibrepower/cross-border-pay/internal/webhooks/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/webhooks/app"
	"github.com/gengibrepower/cross-border-pay/internal/webhooks/domain"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var t0 = time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)

const (
	signingKey = "test-webhook-signing-key-0123456789"
	timeout    = 5 * time.Second
)

var dev = ssrf.Policy{AllowPrivate: true}

// ceilingJitter waits the most full jitter allows, so retry times are exact.
func ceilingJitter(ceiling time.Duration) time.Duration { return ceiling }

type env struct {
	t          *testing.T
	pool       *pgxpool.Pool
	clock      *clock.Fake
	deliveries *eventsadapters.Deliveries
	endpoints  *merchantsapp.Endpoints
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.NewPool(t)
	c := clock.NewFake(t0)
	endpoints, err := merchantsapp.NewEndpoints(merchantsadapters.NewPostgres(pool), dev, []byte(signingKey), c)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, pool: pool, clock: c, deliveries: eventsadapters.NewDeliveries(pool), endpoints: endpoints}
}

// at moves the clock forward to t.
func (e *env) at(t time.Time) { e.clock.Advance(t.Sub(e.clock.Now())) }

func (e *env) deliverer() *app.Deliverer {
	e.t.Helper()
	d, err := app.NewDeliverer(e.deliveries, merchantsadapters.NewPostgres(e.pool), adapters.NewHTTPSender(dev, timeout),
		[]byte(signingKey), e.clock, ceilingJitter, timeout, slog.New(slog.DiscardHandler))
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) merchant() merchants.MerchantID {
	e.t.Helper()
	var id merchants.MerchantID
	if err := e.pool.QueryRow(e.t.Context(), "INSERT INTO merchants (name) VALUES ('m') RETURNING id").Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// endpoint registers url for the merchant and returns its secret.
func (e *env) endpoint(merchant merchants.MerchantID, url string) string {
	e.t.Helper()
	_, secret, err := e.endpoints.Put(e.t.Context(), merchant, merchants.Change{URL: url})
	if err != nil {
		e.t.Fatal(err)
	}
	return secret
}

// event appends an outbox event for the merchant, as payments does, and returns its ID.
func (e *env) event(merchant merchants.MerchantID, n int) [16]byte {
	e.t.Helper()
	var id [16]byte
	_, _ = rand.Read(id[:])
	ev := events.Outgoing{
		ID: id, Merchant: merchant, Type: "payment.paid", Subject: fmt.Sprintf("pay_%d", n),
		Payload: fmt.Appendf(nil, `{"id": "evt_%x", "type": "payment.paid"}`, id), CreatedAt: e.clock.Now(),
	}
	err := postgres.WithTx(e.t.Context(), e.pool, pgx.ReadCommitted, func(ctx context.Context, tx pgx.Tx) error {
		return eventsadapters.Outbox{}.Append(ctx, tx, &ev)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

type row struct {
	status     string
	attempts   int
	next       *time.Time
	statusCode *int
	lastError  *string
	finished   *time.Time
}

func (e *env) row(id [16]byte) row {
	e.t.Helper()
	var r row
	err := e.pool.QueryRow(e.t.Context(),
		"SELECT delivery_status, attempts, next_attempt_at, last_status_code, last_error, finished_at FROM outbox_events WHERE id = $1", id).
		Scan(&r.status, &r.attempts, &r.next, &r.statusCode, &r.lastError, &r.finished)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) pending() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), "SELECT count(*) FROM outbox_events WHERE delivery_status = 'pending'").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// merchantServer plays every merchant's endpoint, one path each: it checks each
// signature with that merchant's secret and counts the deliveries of each event by the
// id in its body, as a merchant deduplicating would.
type merchantServer struct {
	*httptest.Server
	clock    *clock.Fake
	mu       sync.Mutex
	secrets  map[string]string
	received map[string]int
	bodies   [][]byte
	forged   atomic.Int64
	answer   atomic.Int64 // status to answer with; 0 for 200
	// Requests after the holdAfter-th are kept waiting until release closes or the
	// sender gives up; 0 holds none.
	holdAfter atomic.Int64
	release   chan struct{}
	seq, held atomic.Int64
}

func newMerchantServer(t *testing.T, c *clock.Fake) *merchantServer {
	t.Helper()
	m := &merchantServer{clock: c, secrets: map[string]string{}, received: map[string]int{}, release: make(chan struct{})}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Close)
	t.Cleanup(func() { close(m.release) })
	return m
}

// register gives the merchant an endpoint on the server.
func (m *merchantServer) register(e *env, merchant merchants.MerchantID) {
	path := fmt.Sprintf("/%x", merchant)
	secret := e.endpoint(merchant, m.URL+path)
	m.mu.Lock()
	m.secrets[path] = secret
	m.mu.Unlock()
}

func (m *merchantServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	secret := m.secrets[r.URL.Path]
	m.mu.Unlock()
	if domain.Verify(secret, r.Header.Get(domain.SignatureHeader), body, m.clock.Now()) != nil {
		m.forged.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var e struct{ ID string }
	_ = json.Unmarshal(body, &e)
	m.mu.Lock()
	m.received[e.ID]++
	m.bodies = append(m.bodies, body)
	m.mu.Unlock()
	if hold := m.holdAfter.Load(); hold > 0 && m.seq.Add(1) > hold {
		m.held.Add(1)
		select {
		case <-m.release:
		case <-r.Context().Done():
			return
		}
	}
	if code := m.answer.Load(); code != 0 {
		w.WriteHeader(int(code))
	}
}

func (m *merchantServer) times(id [16]byte) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.received[fmt.Sprintf("evt_%x", id)]
}

func TestDeliverSendsTheStoredBytesSigned(t *testing.T) {
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	merchant := e.merchant()
	srv.register(e, merchant)
	id := e.event(merchant, 1)

	n, err := e.deliverer().Deliver(t.Context(), app.DeliverBatch)
	if err != nil || n != 1 {
		t.Fatalf("Deliver() = %d, %v, want 1", n, err)
	}
	if srv.times(id) != 1 || srv.forged.Load() != 0 {
		t.Fatalf("received %d times, %d forged", srv.times(id), srv.forged.Load())
	}
	var stored []byte
	if err := e.pool.QueryRow(t.Context(), "SELECT payload FROM outbox_events WHERE id = $1", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(srv.bodies[0], stored) {
		t.Errorf("sent %s, stored %s", srv.bodies[0], stored)
	}
	r := e.row(id)
	if r.status != "delivered" || r.attempts != 1 || r.next != nil || *r.statusCode != 200 || !r.finished.Equal(t0) {
		t.Errorf("row = %+v, want delivered at the first attempt", r)
	}
	if n, _ := e.deliverer().Deliver(t.Context(), app.DeliverBatch); n != 0 {
		t.Errorf("a delivered event was claimed again")
	}
}

// Decision 23: each failure waits up to RetryBase · 2^(attempt-1); ceilingJitter makes
// it the ceiling, so the times are exact.
func TestDeliverRetriesAFailureWithBackoff(t *testing.T) {
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	srv.answer.Store(http.StatusServiceUnavailable)
	merchant := e.merchant()
	srv.register(e, merchant)
	id := e.event(merchant, 1)
	d := e.deliverer()

	for attempt, wait := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		if n, err := d.Deliver(t.Context(), app.DeliverBatch); n != 1 || err != nil {
			t.Fatalf("attempt %d: Deliver() = %d, %v, want 1", attempt+1, n, err)
		}
		now := e.clock.Now()
		r := e.row(id)
		if r.status != "pending" || r.attempts != attempt+1 || !r.next.Equal(now.Add(wait)) || *r.statusCode != 503 || *r.lastError != "answered 503" {
			t.Fatalf("attempt %d: row = %+v, want pending until %v", attempt+1, r, now.Add(wait))
		}
		e.clock.Advance(wait - time.Microsecond)
		if n, _ := d.Deliver(t.Context(), app.DeliverBatch); n != 0 {
			t.Fatalf("attempt %d: claimed before its retry time", attempt+1)
		}
		e.clock.Advance(time.Microsecond)
	}
	srv.answer.Store(0)
	if n, err := d.Deliver(t.Context(), app.DeliverBatch); n != 1 || err != nil {
		t.Fatalf("Deliver() = %d, %v", n, err)
	}
	if r := e.row(id); r.status != "delivered" || r.attempts != 4 || r.lastError != nil {
		t.Errorf("row = %+v, want delivered at the fourth attempt", r)
	}
	if srv.times(id) != 4 {
		t.Errorf("received %d times, want 4", srv.times(id))
	}
}

func TestDeliverGivesUpAfter72Hours(t *testing.T) {
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	srv.answer.Store(http.StatusInternalServerError)
	merchant := e.merchant()
	srv.register(e, merchant)
	id := e.event(merchant, 1)

	e.clock.Advance(domain.GiveUpAfter - time.Microsecond)
	if _, err := e.deliverer().Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if r := e.row(id); r.status != "pending" {
		t.Fatalf("row = %+v, want pending just before the deadline", r)
	}
	e.clock.Advance(domain.RetryCeiling(1))
	if _, err := e.deliverer().Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	r := e.row(id)
	if r.status != "failed" || r.attempts != 2 || r.next != nil || !r.finished.Equal(e.clock.Now()) || *r.statusCode != 500 {
		t.Errorf("row = %+v, want failed", r)
	}
}

func TestDeliverRetriesAMerchantWithoutEndpoint(t *testing.T) {
	e := newEnv(t)
	id := e.event(e.merchant(), 1)
	if _, err := e.deliverer().Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	r := e.row(id)
	if r.status != "pending" || r.statusCode != nil || *r.lastError != "merchant has no webhook endpoint" || !r.next.Equal(t0.Add(domain.RetryBase)) {
		t.Errorf("row = %+v, want a retry with no status", r)
	}
}

// A redirect and a timeout are failures; neither is followed nor waited out.
func TestDeliverRecordsRedirectAndTimeoutAsFailures(t *testing.T) {
	e := newEnv(t)
	var followed atomic.Int64
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/inside", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/inside", func(http.ResponseWriter, *http.Request) { followed.Add(1) })
	mux.HandleFunc("/slow", func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(release)

	moved, slow := e.merchant(), e.merchant()
	e.endpoint(moved, srv.URL+"/moved")
	e.endpoint(slow, srv.URL+"/slow")
	movedID, slowID := e.event(moved, 1), e.event(slow, 2)

	d, err := app.NewDeliverer(e.deliveries, merchantsadapters.NewPostgres(e.pool), adapters.NewHTTPSender(dev, 200*time.Millisecond),
		[]byte(signingKey), e.clock, ceilingJitter, 200*time.Millisecond, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := d.Deliver(t.Context(), app.DeliverBatch); n != 2 || err != nil {
		t.Fatalf("Deliver() = %d, %v, want 2", n, err)
	}
	if r := e.row(movedID); r.status != "pending" || *r.statusCode != 301 || followed.Load() != 0 {
		t.Errorf("redirect: row = %+v, followed %d times", r, followed.Load())
	}
	if r := e.row(slowID); r.status != "pending" || r.statusCode != nil || r.lastError == nil {
		t.Errorf("timeout: row = %+v, want a retry with no status and an error", r)
	}
}

// attempts fences a claim: once its lease ran out and another worker claimed the
// event, the first can no longer reschedule or fail it. A 2xx is a fact and is still
// recorded, and nothing undoes it.
func TestAStaleClaimCannotRescheduleOrFail(t *testing.T) {
	e := newEnv(t)
	id := e.event(e.merchant(), 1)
	ctx := t.Context()

	first, err := e.deliveries.Claim(ctx, t0, t0.Add(time.Minute), app.PerMerchant, app.DeliverBatch)
	if err != nil || len(first) != 1 {
		t.Fatalf("Claim() = %v, %v", first, err)
	}
	if again, _ := e.deliveries.Claim(ctx, t0, t0.Add(time.Minute), app.PerMerchant, app.DeliverBatch); len(again) != 0 {
		t.Fatal("a leased event was claimed again")
	}
	later := t0.Add(time.Minute)
	second, err := e.deliveries.Claim(ctx, later, later.Add(time.Minute), app.PerMerchant, app.DeliverBatch)
	if err != nil || len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("Claim() after the lease = %v, %v", second, err)
	}

	if ok, err := e.deliveries.Reschedule(ctx, &first[0], 500, "late", later.Add(time.Hour)); ok || err != nil {
		t.Errorf("stale Reschedule() = %v, %v, want false", ok, err)
	}
	if ok, err := e.deliveries.Fail(ctx, &first[0], 500, "late", later); ok || err != nil {
		t.Errorf("stale Fail() = %v, %v, want false", ok, err)
	}
	if r := e.row(id); r.status != "pending" || !r.next.Equal(later.Add(time.Minute)) {
		t.Fatalf("row = %+v, want the second claim's lease untouched", r)
	}
	if ok, err := e.deliveries.Delivered(ctx, &first[0], 200, later); !ok || err != nil {
		t.Errorf("stale Delivered() = %v, %v, want true", ok, err)
	}
	if ok, _ := e.deliveries.Reschedule(ctx, &second[0], 500, "after", later.Add(time.Hour)); ok {
		t.Error("Reschedule() changed a delivered event")
	}
	if r := e.row(id); r.status != "delivered" {
		t.Errorf("row = %+v, want delivered", r)
	}
}

// failed is final, like delivered: only a redelivery by hand brings the event back.
func TestAFailedEventStaysFailed(t *testing.T) {
	e := newEnv(t)
	id := e.event(e.merchant(), 1)
	ctx := t.Context()
	claimed, err := e.deliveries.Claim(ctx, t0, t0.Add(time.Minute), app.PerMerchant, app.DeliverBatch)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %v, %v", claimed, err)
	}
	if ok, err := e.deliveries.Fail(ctx, &claimed[0], 500, "gone", t0); !ok || err != nil {
		t.Fatalf("Fail() = %v, %v", ok, err)
	}
	if ok, err := e.deliveries.Delivered(ctx, &claimed[0], 200, t0); ok || err != nil {
		t.Errorf("Delivered() on a failed event = %v, %v, want false", ok, err)
	}
	if r := e.row(id); r.status != "failed" || *r.statusCode != 500 {
		t.Errorf("row = %+v, want failed as it was", r)
	}
}

// While a merchant switches to a rotated secret, deliveries verify with the old one
// too; PreviousSecretGrace later, only with the new one.
func TestDeliverSignsWithThePreviousSecretWithinTheGrace(t *testing.T) {
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	merchant := e.merchant()
	srv.register(e, merchant) // the server keeps the secret from before the rotation
	_, rotated, err := e.endpoints.Put(t.Context(), merchant, merchants.Change{URL: fmt.Sprintf("%s/%x", srv.URL, merchant), RotateFrom: 1})
	if err != nil || rotated == "" {
		t.Fatalf("rotate: %q, %v", rotated, err)
	}
	d := e.deliverer()

	early := e.event(merchant, 1)
	e.clock.Advance(domain.PreviousSecretGrace - time.Second)
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if r := e.row(early); r.status != "delivered" || srv.forged.Load() != 0 {
		t.Fatalf("within the grace: row = %+v, %d forged", r, srv.forged.Load())
	}

	late := e.event(merchant, 2)
	e.clock.Advance(time.Second)
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if r := e.row(late); r.status != "pending" || *r.statusCode != 401 || srv.forged.Load() != 1 {
		t.Errorf("after the grace: row = %+v, %d forged, want the old secret refused", r, srv.forged.Load())
	}
}

// One merchant's backlog cannot take the whole batch: it gets PerMerchant, and the
// merchant behind it still gets its event in the same claim.
func TestClaimCapsEachMerchant(t *testing.T) {
	e := newEnv(t)
	busy, quiet := e.merchant(), e.merchant()
	for i := range 20 {
		e.event(busy, i)
	}
	e.clock.Advance(time.Second)
	e.event(quiet, 99)

	got, err := e.deliveries.Claim(t.Context(), e.clock.Now(), e.clock.Now().Add(time.Minute), app.PerMerchant, app.DeliverBatch)
	if err != nil {
		t.Fatal(err)
	}
	per := map[[16]byte]int{}
	for _, d := range got {
		per[d.Event.Merchant]++
	}
	if per[busy] != app.PerMerchant || per[quiet] != 1 {
		t.Errorf("claimed %d of the busy merchant and %d of the quiet one, want %d and 1", per[busy], per[quiet], app.PerMerchant)
	}
}

// Workers claiming at once never take the same event: with no failure and no lease
// running out, each event is posted exactly once.
func TestConcurrentWorkersPostEachEventOnce(t *testing.T) {
	const workers, merchantCount, perMerchant = 4, 10, 30
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	var ids [][16]byte
	for m := range merchantCount {
		merchant := e.merchant()
		srv.register(e, merchant)
		for i := range perMerchant {
			ids = append(ids, e.event(merchant, m*perMerchant+i))
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		d := e.deliverer()
		wg.Go(func() {
			for {
				n, err := d.Deliver(t.Context(), app.DeliverBatch)
				if err != nil {
					errs <- err
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if p := e.pending(); p != 0 {
		t.Fatalf("%d events still pending", p)
	}
	for _, id := range ids {
		if n := srv.times(id); n != 1 {
			t.Errorf("event %x posted %d times, want 1", id, n)
		}
	}
	if srv.forged.Load() != 0 {
		t.Errorf("%d posts with a bad signature", srv.forged.Load())
	}
}

// Test 4 of section 9: the worker dies in the middle of a round, with posts in flight
// that the merchant received but the worker never saw answered. After a restart, once
// their lease runs out, every event is delivered at least once: those cut off twice,
// the merchant dropping the copy by its id, and every other one once.
func TestWorkerKilledMidRoundDeliversEveryEventAfterRestart(t *testing.T) {
	const merchantCount, perMerchant = 5, 20
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	var ids [][16]byte
	for m := range merchantCount {
		merchant := e.merchant()
		srv.register(e, merchant)
		for i := range perMerchant {
			ids = append(ids, e.event(merchant, m*perMerchant+i))
		}
	}
	// A round takes PerMerchant of each merchant: 20 posts. The first round goes
	// through, and the second is held after its first 10.
	round := int64(app.PerMerchant * merchantCount)
	held := round + round/2
	srv.holdAfter.Store(held)

	ctx, kill := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		d := e.deliverer()
		for ctx.Err() == nil {
			_, _ = d.Deliver(ctx, app.DeliverBatch)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for srv.held.Load() < round/2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d posts held after 10 s, want %d", srv.held.Load(), round/2)
		}
		time.Sleep(time.Millisecond)
	}
	kill()
	<-stopped

	if p := e.pending(); p != len(ids)-int(held) {
		t.Fatalf("%d pending after the kill, want %d", p, len(ids)-int(held))
	}
	var cut [][16]byte
	for _, id := range ids {
		if r := e.row(id); srv.times(id) == 1 && r.status == "pending" {
			cut = append(cut, id)
			// Cut short by the kill, not refused: nothing recorded, the lease stands.
			if r.lastError != nil || !r.next.Equal(t0.Add(timeout+app.LeaseMargin)) {
				t.Errorf("cut-off event %x: row = %+v, want only its lease", id, r)
			}
		}
	}
	if len(cut) != int(round/2) {
		t.Fatalf("%d events received but not recorded, want %d", len(cut), round/2)
	}

	srv.holdAfter.Store(0)
	e.clock.Advance(timeout + app.LeaseMargin)
	restarted := e.deliverer()
	for range 100 {
		n, err := restarted.Deliver(t.Context(), app.DeliverBatch)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if p := e.pending(); p != 0 {
		t.Fatalf("%d events still pending after the restart", p)
	}
	for _, id := range cut {
		if n := srv.times(id); n != 2 {
			t.Errorf("cut-off event %x received %d times, want 2", id, n)
		}
	}
	twice := 0
	for _, id := range ids {
		switch srv.times(id) {
		case 0:
			t.Errorf("event %x never received", id)
		case 2:
			twice++
		}
	}
	if twice != len(cut) {
		t.Errorf("%d events received twice, want only the %d cut off", twice, len(cut))
	}
}

func (m *merchantServer) posts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.received {
		n += c
	}
	return n
}

// Decision 24 end to end: 72 h of only failures disable the endpoint, which then gets
// no post at all, its events waiting; a PUT by the merchant turns it back on.
func TestADeadEndpointIsDisabledUntilTheMerchantChangesIt(t *testing.T) {
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	srv.answer.Store(http.StatusBadGateway)
	merchant := e.merchant()
	srv.register(e, merchant)
	d := e.deliverer()
	get := func() merchants.Endpoint {
		t.Helper()
		ep, err := e.endpoints.Get(t.Context(), merchant)
		if err != nil {
			t.Fatal(err)
		}
		return ep
	}

	e.event(merchant, 1)
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(merchants.DisableAfter - time.Second)
	e.event(merchant, 2)
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if ep := get(); ep.Disabled() {
		t.Fatal("disabled before 72 h of failures")
	}
	e.clock.Advance(time.Second)
	e.event(merchant, 3)
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	ep := get()
	if !ep.Disabled() || ep.DisabledReason != "only failures for 72 h; last: answered 502" {
		t.Fatalf("endpoint = %+v, want disabled at 72 h of failures", ep)
	}

	posts := srv.posts()
	late := e.event(merchant, 4)
	if n, err := d.Deliver(t.Context(), app.DeliverBatch); n != 1 || err != nil {
		t.Fatalf("Deliver() = %d, %v, want the new event claimed", n, err)
	}
	if srv.posts() != posts {
		t.Fatal("posted to a disabled endpoint")
	}
	r := e.row(late)
	if r.status != "pending" || r.statusCode != nil || !strings.HasPrefix(*r.lastError, "webhook endpoint disabled") {
		t.Fatalf("row = %+v, want waiting on the disabled endpoint", r)
	}

	srv.answer.Store(0)
	e.endpoint(merchant, fmt.Sprintf("%s/%x", srv.URL, merchant)) // the same URL: the merchant says it is fixed
	if ep := get(); ep.Disabled() {
		t.Fatal("still disabled after a PUT")
	}
	e.clock.Advance(domain.RetryCeiling(r.attempts))
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if r := e.row(late); r.status != "delivered" || srv.times(late) != 1 {
		t.Errorf("row = %+v, received %d times; want delivered once turned back on", r, srv.times(late))
	}
}

// A round where any post to the merchant succeeds ends its run of failures, even if
// others failed in the same round.
func TestAnyDeliveredPostEndsTheRunOfFailures(t *testing.T) {
	e := newEnv(t)
	merchant := e.merchant()
	mux := http.NewServeMux()
	var n atomic.Int64
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if n.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e.endpoint(merchant, srv.URL)
	if _, err := merchantsadapters.NewPostgres(e.pool).EndpointFailed(t.Context(), merchant, "answered 500", t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		e.event(merchant, i)
	}
	if _, err := e.deliverer().Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	ep, err := e.endpoints.Get(t.Context(), merchant)
	if err != nil || !ep.FailingSince.IsZero() {
		t.Errorf("endpoint = %+v, %v; want the run of failures ended", ep, err)
	}
}

func TestCleanupDeletesOnlyDeliveredEventsPastTheRetention(t *testing.T) {
	e := newEnv(t)
	merchant := e.merchant()
	// After the clock advances by the 30-day retention, the cutoff is t0: old sits on it and
	// fresh a microsecond past it.
	old := t0
	finish := func(id [16]byte, status string, at time.Time) {
		t.Helper()
		_, err := e.pool.Exec(t.Context(),
			"UPDATE outbox_events SET delivery_status = $2, next_attempt_at = NULL, finished_at = $3 WHERE id = $1", id, status, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	var deleted, kept [][16]byte
	for i := range 5 {
		id := e.event(merchant, i)
		finish(id, "delivered", old)
		deleted = append(deleted, id)
	}
	fresh := e.event(merchant, 10)
	finish(fresh, "delivered", old.Add(time.Microsecond))
	failed := e.event(merchant, 11)
	finish(failed, "failed", old)
	kept = append(kept, fresh, failed, e.event(merchant, 12))

	// Thirty days, written out rather than read from the constant it checks.
	e.clock.Advance(30 * 24 * time.Hour)
	n, err := e.deliverer().Cleanup(t.Context(), 2)
	if err != nil || n != 5 {
		t.Fatalf("Cleanup() = %d, %v, want the 5 old delivered ones over 3 batches", n, err)
	}
	var left int
	if err := e.pool.QueryRow(t.Context(), "SELECT count(*) FROM outbox_events WHERE id = ANY($1)", deleted).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d old delivered events left, %v", left, err)
	}
	if err := e.pool.QueryRow(t.Context(), "SELECT count(*) FROM outbox_events WHERE id = ANY($1)", kept).Scan(&left); err != nil || left != 3 {
		t.Errorf("%d of the recent, failed and pending events kept, want 3 (%v)", left, err)
	}
}

// A failed event redelivered by hand gets a new window: the backoff restarts from
// RetryBase instead of the 6 h ceiling the old attempts reached, and the 72 h count
// from the redelivery.
func TestARedeliveredEventRetriesInANewWindow(t *testing.T) {
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	srv.answer.Store(http.StatusInternalServerError)
	merchant := e.merchant()
	srv.register(e, merchant)
	id := e.event(merchant, 1)
	d := e.deliverer()

	for e.row(id).status == "pending" {
		if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
			t.Fatal(err)
		}
		if r := e.row(id); r.next != nil {
			e.at(*r.next)
		}
	}
	gaveUp := e.row(id)
	if gaveUp.status != "failed" || gaveUp.attempts < 20 {
		t.Fatalf("row = %+v, want failed after the whole window", gaveUp)
	}

	// The same 72 h of failures disabled the endpoint (decision 24): the merchant fixes
	// it with a PUT before redelivering, which the API asks for.
	if ep, _ := e.endpoints.Get(t.Context(), merchant); !ep.Disabled() {
		t.Fatalf("endpoint = %+v, want disabled by the same failures", ep)
	}
	e.clock.Advance(time.Hour)
	e.endpoint(merchant, fmt.Sprintf("%s/%x", srv.URL, merchant))
	redelivered := e.clock.Now()
	if _, err := e.deliveries.Redeliver(t.Context(), merchant, id, redelivered); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	r := e.row(id)
	if r.status != "pending" || r.attempts != gaveUp.attempts+1 || !r.next.Equal(redelivered.Add(domain.RetryBase)) {
		t.Fatalf("row = %+v, want pending until %v: the backoff restarted", r, redelivered.Add(domain.RetryBase))
	}
	e.at(redelivered.Add(domain.GiveUpAfter - time.Microsecond))
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if r := e.row(id); r.status != "pending" {
		t.Fatalf("row = %+v, want pending within 72 h of the redelivery", r)
	}

	srv.answer.Store(0)
	e.at(*e.row(id).next)
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if r := e.row(id); r.status != "delivered" || r.lastError != nil {
		t.Errorf("row = %+v, want delivered", r)
	}
}

// A delivered event redelivered is sent again, the same bytes, for a merchant that
// lost it.
func TestARedeliveredDeliveredEventIsSentAgain(t *testing.T) {
	e := newEnv(t)
	srv := newMerchantServer(t, e.clock)
	merchant := e.merchant()
	srv.register(e, merchant)
	id := e.event(merchant, 1)
	d := e.deliverer()
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deliveries.Redeliver(t.Context(), merchant, id, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deliver(t.Context(), app.DeliverBatch); err != nil {
		t.Fatal(err)
	}
	if srv.times(id) != 2 || e.row(id).status != "delivered" || !bytes.Equal(srv.bodies[0], srv.bodies[1]) {
		t.Errorf("received %d times, row %+v; want twice, the same bytes", srv.times(id), e.row(id))
	}
}

// A post in flight when the merchant redelivers must not push the "due now" back with
// its own backoff: its claim is from before the window and is fenced off.
func TestAClaimFromBeforeARedeliveryCannotReschedule(t *testing.T) {
	e := newEnv(t)
	merchant := e.merchant()
	id := e.event(merchant, 1)
	ctx := t.Context()
	inFlight, err := e.deliveries.Claim(ctx, t0, t0.Add(time.Minute), app.PerMerchant, app.DeliverBatch)
	if err != nil || len(inFlight) != 1 {
		t.Fatalf("Claim() = %v, %v", inFlight, err)
	}
	if _, err := e.deliveries.Redeliver(ctx, merchant, id, t0); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.deliveries.Reschedule(ctx, &inFlight[0], 500, "late", t0.Add(time.Hour)); ok || err != nil {
		t.Errorf("Reschedule() from before the redelivery = %v, %v, want false", ok, err)
	}
	if ok, _ := e.deliveries.Fail(ctx, &inFlight[0], 500, "late", t0); ok {
		t.Error("Fail() from before the redelivery changed the event")
	}
	again, err := e.deliveries.Claim(ctx, t0, t0.Add(time.Minute), app.PerMerchant, app.DeliverBatch)
	if err != nil || len(again) != 1 || again[0].WindowAttempt() != 1 || !again[0].WindowStart.Equal(t0) {
		t.Fatalf("Claim() after the redelivery = %+v, %v; want the first attempt of the new window", again, err)
	}
	if ok, err := e.deliveries.Reschedule(ctx, &again[0], 500, "now", t0.Add(time.Hour)); !ok || err != nil {
		t.Errorf("Reschedule() of the new claim = %v, %v, want true", ok, err)
	}
}

func TestRedeliverFindsOnlyTheMerchantsOwnEvent(t *testing.T) {
	e := newEnv(t)
	id := e.event(e.merchant(), 1)
	if _, err := e.deliveries.Redeliver(t.Context(), e.merchant(), id, t0); !errors.Is(err, events.ErrOutgoingNotFound) {
		t.Errorf("Redeliver() of another merchant's event = %v, want %v", err, events.ErrOutgoingNotFound)
	}
}
