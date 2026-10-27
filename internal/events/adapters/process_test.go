package adapters_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/events/app"
	"github.com/gengibrepower/cross-border-pay/internal/events/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

// scripted writes one effect row per call inside the event's transaction, then answers
// what result says, so a test sees whether the effect committed with the event.
type scripted struct {
	result func(e *domain.Event) (app.Result, error)

	mu    sync.Mutex
	calls map[string]int
}

func (h *scripted) Handle(ctx context.Context, tx pgx.Tx, e *domain.Event) (app.Result, error) {
	h.mu.Lock()
	h.calls[e.ExternalID]++
	h.mu.Unlock()
	if _, err := tx.Exec(ctx, "INSERT INTO effects (external_id) VALUES ($1)", e.ExternalID); err != nil {
		return app.Result{}, err //nolint:wrapcheck // a test double passes the database's error through untouched.
	}
	return h.result(e)
}

type queue struct {
	t         *testing.T
	pool      *pgxpool.Pool
	store     *adapters.Postgres
	clock     *clock.Fake
	handler   *scripted
	processor *app.Processor
}

func newQueue(t *testing.T, result func(e *domain.Event) (app.Result, error)) *queue {
	t.Helper()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(t.Context(), "CREATE TABLE effects (external_id text NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	q := &queue{
		t: t, pool: pool, store: adapters.NewPostgres(pool), clock: clock.NewFake(t0),
		handler: &scripted{result: result, calls: map[string]int{}},
	}
	q.processor = app.NewProcessor(q.store, q.handler, q.clock, slog.New(slog.DiscardHandler))
	return q
}

func (q *queue) receive(ids ...string) {
	q.t.Helper()
	for _, id := range ids {
		in := domain.Incoming{Provider: "sim", ExternalID: id, Type: "test.happened", Payload: []byte(`{}`), Signature: "ok"}
		if _, _, err := q.store.Insert(q.t.Context(), &in, q.clock.Now()); err != nil {
			q.t.Fatal(err)
		}
	}
}

func (q *queue) process() int {
	q.t.Helper()
	n, err := q.processor.Process(q.t.Context(), app.ProcessBatch)
	if err != nil {
		q.t.Fatalf("Process() error = %v", err)
	}
	return n
}

type row struct {
	status    string
	attempts  int
	next      *time.Time
	lastError *string
	subject   *string
	processed *time.Time
}

func (q *queue) row(id string) row {
	q.t.Helper()
	var r row
	err := q.pool.QueryRow(q.t.Context(), `SELECT status, attempts, next_attempt_at, last_error, subject_id, processed_at
		FROM inbox_events WHERE external_id = $1`, id).Scan(&r.status, &r.attempts, &r.next, &r.lastError, &r.subject, &r.processed)
	if err != nil {
		q.t.Fatal(err)
	}
	return r
}

func (q *queue) effects(id string) int {
	q.t.Helper()
	var n int
	if err := q.pool.QueryRow(q.t.Context(), "SELECT count(*) FROM effects WHERE external_id = $1", id).Scan(&n); err != nil {
		q.t.Fatal(err)
	}
	return n
}

func byID(results map[string]app.Result) func(e *domain.Event) (app.Result, error) {
	return func(e *domain.Event) (app.Result, error) { return results[e.ExternalID], nil }
}

func TestProcessRecordsEachOutcome(t *testing.T) {
	q := newQueue(t, byID(map[string]app.Result{
		"processed": {Outcome: domain.Processed, Subject: "att_1"},
		"ignored":   {Outcome: domain.Ignored},
		"failed":    {Outcome: domain.Failed, Reason: "paid twice the quote"},
		"retry":     {Outcome: domain.Retry, Reason: "attempt not stored yet"},
	}))
	q.receive("processed", "ignored", "failed", "retry")
	q.clock.Advance(time.Second)
	if n := q.process(); n != 4 {
		t.Fatalf("processed %d events, want 4", n)
	}

	now := q.clock.Now()
	for id, want := range map[string]string{"processed": "processed", "ignored": "ignored", "failed": "failed"} {
		r := q.row(id)
		if r.status != want || r.next != nil || r.processed == nil || !r.processed.Equal(now) {
			t.Errorf("%s: %+v, want %s and finished now", id, r, want)
		}
	}
	if r := q.row("processed"); r.subject == nil || *r.subject != "att_1" {
		t.Errorf("processed subject = %v, want att_1", r.subject)
	}
	if r := q.row("failed"); r.lastError == nil || *r.lastError != "paid twice the quote" {
		t.Errorf("failed reason = %v", r.lastError)
	}
	r := q.row("retry")
	if r.status != "pending" || r.attempts != 1 || r.next == nil || !r.next.Equal(now.Add(domain.RetryBase)) || r.processed != nil ||
		r.lastError == nil || *r.lastError != "attempt not stored yet" {
		t.Errorf("retry: %+v, want pending, 1 attempt, next in %v", r, domain.RetryBase)
	}
}

func TestRetryWaitsItsBackoff(t *testing.T) {
	q := newQueue(t, byID(map[string]app.Result{"evt": {Outcome: domain.Retry}}))
	q.receive("evt")
	q.process()

	q.clock.Advance(domain.RetryBackoff(0) - time.Second)
	if n := q.process(); n != 0 {
		t.Fatalf("claimed %d events before the retry was due", n)
	}
	q.clock.Advance(time.Second)
	if n := q.process(); n != 1 {
		t.Fatalf("claimed %d events once due, want 1", n)
	}
	if r := q.row("evt"); r.attempts != 2 || !r.next.Equal(q.clock.Now().Add(domain.RetryBackoff(1))) {
		t.Errorf("after two tries: %+v, want next in %v", r, domain.RetryBackoff(1))
	}
}

// The handler's effect and the event's status commit together: a handler that fails
// after writing leaves no effect, and the event waits for another try.
func TestHandlerFailureRollsBackItsEffect(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	q := newQueue(t, func(*domain.Event) (app.Result, error) {
		if fail.Load() {
			return app.Result{}, errors.New("lost the connection")
		}
		return app.Result{Outcome: domain.Processed}, nil
	})
	q.receive("evt")

	if _, err := q.processor.Process(t.Context(), app.ProcessBatch); err == nil {
		t.Fatal("Process() hid the handler's error")
	}
	r := q.row("evt")
	if q.effects("evt") != 0 || r.status != "pending" || r.attempts != 1 || r.lastError == nil || !r.next.Equal(t0.Add(domain.RetryBase)) {
		t.Fatalf("after a failed handler: %d effects, %+v; want none and a retry", q.effects("evt"), r)
	}

	fail.Store(false)
	q.clock.Advance(domain.RetryBase)
	q.process()
	if q.effects("evt") != 1 || q.row("evt").status != "processed" {
		t.Errorf("after recovering: %d effects, %s; want 1 and processed", q.effects("evt"), q.row("evt").status)
	}
}

func TestRetryGivesUpAfterADay(t *testing.T) {
	q := newQueue(t, byID(map[string]app.Result{"evt": {Outcome: domain.Retry, Reason: "attempt not stored yet"}}))
	q.receive("evt")
	q.clock.Advance(domain.GiveUpAfter)
	q.process()
	if r := q.row("evt"); r.status != "failed" || r.next != nil {
		t.Errorf("a day later: %+v, want failed and out of the queue", r)
	}
	if n := q.process(); n != 0 {
		t.Errorf("a failed event was claimed again")
	}
}

// SKIP LOCKED: an event another transaction holds is passed over, not waited on.
func TestALockedEventDoesNotBlockTheOthers(t *testing.T) {
	q := newQueue(t, byID(map[string]app.Result{
		"held": {Outcome: domain.Processed}, "free": {Outcome: domain.Processed},
	}))
	q.receive("held", "free")
	tx, err := q.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := tx.Exec(t.Context(), "SELECT 1 FROM inbox_events WHERE external_id = 'held' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	n, err := q.processor.Process(ctx, app.ProcessBatch)
	if err != nil || n != 1 || q.row("free").status != "processed" || q.row("held").status != "pending" {
		t.Errorf("Process() = %d, %v; free %s, held %s; want the free one only", n, err, q.row("free").status, q.row("held").status)
	}
}

func TestConcurrentProcessorsApplyEachEventOnce(t *testing.T) {
	q := newQueue(t, func(*domain.Event) (app.Result, error) { return app.Result{Outcome: domain.Processed}, nil })
	const events, processors = 50, 4
	ids := make([]string, events)
	for i := range ids {
		ids[i] = "evt_" + string(rune('A'+i/26)) + string(rune('a'+i%26))
	}
	q.receive(ids...)

	var taken atomic.Int64
	var wg sync.WaitGroup
	for range processors {
		wg.Go(func() {
			for {
				n, err := q.processor.Process(t.Context(), app.ProcessBatch)
				if err != nil {
					t.Errorf("Process() error = %v", err)
					return
				}
				if n == 0 {
					return
				}
				taken.Add(int64(n))
			}
		})
	}
	wg.Wait()

	if n := taken.Load(); n != events {
		t.Errorf("%d events taken, want %d", n, events)
	}
	for _, id := range ids {
		if c, e := q.handler.calls[id], q.effects(id); c != 1 || e != 1 {
			t.Errorf("%s: %d calls and %d effects, want 1 and 1", id, c, e)
		}
	}
}

// One event whose handler fails does not hold back the rest of the batch.
func TestAFailingEventDoesNotStopTheBatch(t *testing.T) {
	q := newQueue(t, func(e *domain.Event) (app.Result, error) {
		if e.ExternalID == "poison" {
			return app.Result{}, errors.New("cannot apply")
		}
		return app.Result{Outcome: domain.Processed}, nil
	})
	q.receive("poison")
	q.clock.Advance(time.Second)
	q.receive("fine")
	q.clock.Advance(time.Second)

	n, err := q.processor.Process(t.Context(), app.ProcessBatch)
	if err == nil || n != 2 || q.row("fine").status != "processed" || q.row("poison").status != "pending" {
		t.Errorf("Process() = %d, %v; fine %s, poison %s; want both taken and only fine processed",
			n, err, q.row("fine").status, q.row("poison").status)
	}
}

// Retry runs without the row lock, after the failed transaction; if another worker
// finished the event meanwhile, the late Retry must not reopen it.
func TestLateRetryDoesNotReopenAFinishedEvent(t *testing.T) {
	q := newQueue(t, byID(map[string]app.Result{"evt": {Outcome: domain.Processed}}))
	q.receive("evt")
	var stale domain.Event
	if err := q.pool.QueryRow(t.Context(), "SELECT id FROM inbox_events").Scan(&stale.ID); err != nil {
		t.Fatal(err)
	}
	stale.ReceivedAt, stale.ExternalID = t0, "evt"
	q.process()

	if _, err := q.store.Retry(t.Context(), &stale, "lost the connection", q.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if r := q.row("evt"); r.status != "processed" || r.attempts != 0 || r.lastError != nil {
		t.Errorf("after a late retry: %+v, want still processed", r)
	}
	stale.ReceivedAt = t0.Add(-domain.GiveUpAfter)
	if _, err := q.store.Retry(t.Context(), &stale, "lost the connection", q.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if r := q.row("evt"); r.status != "processed" {
		t.Errorf("after a late retry past the limit: %s, want still processed", r.status)
	}
}
