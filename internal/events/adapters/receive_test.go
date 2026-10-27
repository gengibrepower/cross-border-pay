package adapters_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/events/app"
	"github.com/gengibrepower/cross-border-pay/internal/events/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// stubVerifier accepts the signature "ok" and names the event by X-Event-Id.
type stubVerifier struct{}

func (stubVerifier) Verify(header func(string) string, body []byte, _ time.Time) (domain.Incoming, error) {
	switch header("X-Signature") {
	case "ok":
		return domain.Incoming{ExternalID: header("X-Event-Id"), Type: "test.happened", Payload: body, Signature: "ok"}, nil
	case "unreadable":
		return domain.Incoming{}, app.ErrMalformedEvent
	default:
		return domain.Incoming{}, app.ErrUnauthenticated
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type receiver struct {
	pool    *pgxpool.Pool
	handler http.Handler
	log     *syncBuffer
}

func newReceiver(t *testing.T, store app.Store) *receiver {
	t.Helper()
	pool := pgtest.NewPool(t)
	if store == nil {
		store = adapters.NewPostgres(pool)
	}
	log := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(log, nil))
	inbox := app.NewInbox(store, map[string]app.Verifier{"sim": stubVerifier{}}, clock.NewFake(t0), logger)
	mux := http.NewServeMux()
	adapters.NewHandler(inbox, logger).Register(mux)
	return &receiver{pool: pool, handler: mux, log: log}
}

func (r *receiver) post(t *testing.T, provider, signature, eventID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/providers/"+provider+"/events", strings.NewReader(body))
	req.Header.Set("X-Signature", signature)
	req.Header.Set("X-Event-Id", eventID)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

func (r *receiver) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(t.Context(), "SELECT count(*) FROM inbox_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReceiveStoresTheEventAsSent(t *testing.T) {
	r := newReceiver(t, nil)
	body := `{"id":"evt_1","amount":{"value":10150}}`
	if rec := r.post(t, "sim", "ok", "evt_1", body); rec.Code != http.StatusOK || rec.Body.String() != `{"status":"received"}` {
		t.Fatalf("got %d %s, want 200", rec.Code, rec.Body)
	}
	var provider, externalID, typ, signature, status string
	var payload []byte
	var next, received time.Time
	err := r.pool.QueryRow(t.Context(), `SELECT provider, external_id, type, payload, signature, status, next_attempt_at, received_at
		FROM inbox_events`).Scan(&provider, &externalID, &typ, &payload, &signature, &status, &next, &received)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "sim" || externalID != "evt_1" || typ != "test.happened" || string(payload) != body || signature != "ok" ||
		status != "pending" || !next.Equal(t0) || !received.Equal(t0) {
		t.Errorf("stored %s %s %s %q %s %s %v %v", provider, externalID, typ, payload, signature, status, next, received)
	}
}

func TestReceiveRefusesWhatItCannotTrust(t *testing.T) {
	r := newReceiver(t, nil)
	for _, tc := range []struct {
		name      string
		provider  string
		signature string
		eventID   string
		body      string
		status    int
	}{
		{"bad signature", "sim", "forged", "evt_1", "{}", http.StatusUnauthorized},
		{"unreadable", "sim", "unreadable", "evt_1", "{}", http.StatusBadRequest},
		{"no event ID", "sim", "ok", "", "{}", http.StatusBadRequest},
		{"unknown provider", "other", "ok", "evt_1", "{}", http.StatusNotFound},
		{"too large", "sim", "ok", "evt_1", strings.Repeat("x", 64<<10+1), http.StatusRequestEntityTooLarge},
	} {
		if rec := r.post(t, tc.provider, tc.signature, tc.eventID, tc.body); rec.Code != tc.status {
			t.Errorf("%s: got %d %s, want %d", tc.name, rec.Code, rec.Body, tc.status)
		}
	}
	if n := r.count(t); n != 0 {
		t.Errorf("%d events stored from refused requests", n)
	}
}

// The provider retries on anything but 2xx, so 100 deliveries of one event at once are
// all answered 200, and the unique key keeps one row.
func TestConcurrentDeliveriesStoreOneEvent(t *testing.T) {
	r := newReceiver(t, nil)
	const deliveries = 100
	codes := make([]int, deliveries)
	var wg sync.WaitGroup
	for i := range deliveries {
		wg.Go(func() { codes[i] = r.post(t, "sim", "ok", "evt_1", `{"id":"evt_1"}`).Code })
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("delivery %d answered %d, want 200", i, code)
		}
	}
	if n := r.count(t); n != 1 {
		t.Errorf("%d rows for one event, want 1", n)
	}
}

func TestRedeliveryWithAnotherPayloadKeepsTheFirst(t *testing.T) {
	r := newReceiver(t, nil)
	r.post(t, "sim", "ok", "evt_1", `{"value":1}`)
	if rec := r.post(t, "sim", "ok", "evt_1", `{"value":2}`); rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var payload []byte
	if err := r.pool.QueryRow(t.Context(), "SELECT payload FROM inbox_events").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"value":1}` || !strings.Contains(r.log.String(), "event redelivered with a different payload") {
		t.Errorf("payload %s, log %s; want the first payload and an error logged", payload, r.log)
	}
	r.post(t, "sim", "ok", "evt_1", `{"value":1}`)
	if n := strings.Count(r.log.String(), "different payload"); n != 1 {
		t.Errorf("an identical redelivery was logged as different (%d logs)", n)
	}
}

type failingStore struct{}

func (failingStore) Insert(context.Context, *domain.Incoming, time.Time) (inserted bool, stored []byte, err error) {
	return false, nil, errors.New("database unavailable")
}

// A failure to store must make the provider deliver again.
func TestReceiveAsksForRedeliveryWhenStoringFails(t *testing.T) {
	r := newReceiver(t, failingStore{})
	if rec := r.post(t, "sim", "ok", "evt_1", "{}"); rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500", rec.Code)
	}
}
