package simkit_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

var t0 = time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)

type fixed time.Time

func (f fixed) Now() time.Time { return time.Time(f) }

func newFaults() *simkit.Faults {
	return simkit.NewFaults([]simkit.Op{"create", "get"}, []simkit.Op{"create"})
}

// Faults run in the order they were added, each for its count, and only for its op.
func TestFaultsRunInOrderForTheirCount(t *testing.T) {
	q := newFaults()
	for _, f := range []simkit.Fault{
		{Op: "create", Kind: simkit.FailBefore, Count: 2},
		{Op: "get", Kind: simkit.TimeoutAfter, Count: 1},
		{Op: "create", Kind: simkit.Reject, Count: 1},
	} {
		if err := q.Add(&f); err != nil {
			t.Fatal(err)
		}
	}
	want := []simkit.FaultKind{simkit.FailBefore, simkit.FailBefore, simkit.Reject, ""}
	for i, k := range want {
		if got := q.Next("create").Kind; got != k {
			t.Errorf("create #%d = %q, want %q", i+1, got, k)
		}
	}
	if got := q.Next("get").Kind; got != simkit.TimeoutAfter {
		t.Errorf("get = %q, want %q", got, simkit.TimeoutAfter)
	}
	if got := q.Next("get").Kind; got != "" {
		t.Errorf("get after its fault = %q, want none", got)
	}
}

// Concurrent requests share the uses of a fault: never more than its count.
func TestFaultsAreTakenOnceUnderConcurrency(t *testing.T) {
	q := newFaults()
	if err := q.Add(&simkit.Fault{Op: "create", Kind: simkit.FailBefore, Count: 10}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	taken := 0
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if q.Next("create").Kind == simkit.FailBefore {
				mu.Lock()
				taken++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if taken != 10 {
		t.Errorf("fault taken %d times, want 10", taken)
	}
}

func TestFaultsRefuseWhatTheSimulatorCannotDo(t *testing.T) {
	q := newFaults()
	for name, f := range map[string]simkit.Fault{
		"unknown op":      {Op: "refund", Kind: simkit.FailBefore, Count: 1},
		"reject a get":    {Op: "get", Kind: simkit.Reject, Count: 1},
		"unknown kind":    {Op: "create", Kind: "explode", Count: 1},
		"no count":        {Op: "create", Kind: simkit.FailBefore},
		"count too big":   {Op: "create", Kind: simkit.FailBefore, Count: simkit.MaxFaultCount + 1},
		"delay of zero":   {Op: "create", Kind: simkit.Delay, Count: 1},
		"delay too long":  {Op: "create", Kind: simkit.Delay, Count: 1, DelayMS: simkit.MaxDelay.Milliseconds() + 1},
		"delay on a fail": {Op: "create", Kind: simkit.FailBefore, Count: 1, DelayMS: 10},
	} {
		if err := q.Add(&f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if f := q.Next("create"); f.Kind != "" {
		t.Errorf("a refused fault was scheduled: %+v", f)
	}
}

// The same seed and key give the same draws; another seed or key, others.
func TestSeedDrawsByKey(t *testing.T) {
	a := simkit.Text(simkit.Seed(1).RNG("k"), 20)
	if b := simkit.Text(simkit.Seed(1).RNG("k"), 20); a != b {
		t.Errorf("same seed and key: %q then %q", a, b)
	}
	if b := simkit.Text(simkit.Seed(2).RNG("k"), 20); a == b {
		t.Errorf("seeds 1 and 2 drew the same %q", a)
	}
	if b := simkit.Text(simkit.Seed(1).RNG("j"), 20); a == b {
		t.Errorf("keys k and j drew the same %q", a)
	}
}

func TestSignatureRoundTripAndForgeries(t *testing.T) {
	secret, body := []byte("secret"), []byte(`{"id":"evt_1"}`)
	header := simkit.Sign(secret, t0, body)
	if err := simkit.Verify(secret, header, body, t0.Add(time.Minute)); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	for name, tc := range map[string]struct {
		secret []byte
		body   []byte
		now    time.Time
		want   error
	}{
		"another secret":  {[]byte("other"), body, t0, simkit.ErrBadSignature},
		"another body":    {secret, []byte(`{"id":"evt_2"}`), t0, simkit.ErrBadSignature},
		"too old":         {secret, body, t0.Add(simkit.SignatureTolerance + time.Second), simkit.ErrStaleSignature},
		"from the future": {secret, body, t0.Add(-simkit.SignatureTolerance - time.Second), simkit.ErrStaleSignature},
	} {
		if err := simkit.Verify(tc.secret, header, tc.body, tc.now); !errors.Is(err, tc.want) {
			t.Errorf("%s: Verify() = %v, want %v", name, err, tc.want)
		}
	}
}

// A redelivery sends the bytes first recorded, signed again; an unknown event is 404.
func TestWebhookRedeliversTheRecordedBytes(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := simkit.Verify([]byte("s"), r.Header.Get("Test-Signature"), body, t0); err != nil {
			t.Errorf("delivery signature: %v", err)
		}
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
	}))
	defer srv.Close()

	h, err := simkit.NewWebhook(srv.URL, []byte("s"), "Test-Signature", fixed(t0))
	if err != nil {
		t.Fatal(err)
	}
	body := h.Record("evt_1", map[string]string{"id": "evt_1"})
	if err := h.Deliver(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /control/events/{id}/deliver", h.Redeliver)
	for id, want := range map[string]int{"evt_1": http.StatusNoContent, "evt_2": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/control/events/"+id+"/deliver", http.NoBody))
		if rec.Code != want {
			t.Errorf("redeliver %s = %d, want %d", id, rec.Code, want)
		}
	}
	if len(got) != 2 || !bytes.Equal(got[0], got[1]) {
		t.Errorf("deliveries = %q, want the same bytes twice", got)
	}

	off, err := simkit.NewWebhook("", nil, "Test-Signature", fixed(t0))
	if err != nil {
		t.Fatal(err)
	}
	if b := off.Record("evt_1", map[string]string{}); b != nil {
		t.Errorf("Record() without a URL = %q, want nil", b)
	}
	if _, err := simkit.NewWebhook(srv.URL, nil, "Test-Signature", fixed(t0)); err == nil {
		t.Error("a webhook URL without a secret was accepted")
	}
}

// Only the problems the simulator knows become its errors; any other answer is
// ErrBadResponse, so a 500 never reads as a refusal.
func TestClientMapsOnlyKnownProblems(t *testing.T) {
	errRefused := errors.New("refused")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/refused":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"code":"refused"}`)
		case "/other":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"code":"something_else"}`)
		case "/down":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			if r.Header.Get("Idempotency-Key") != "k" {
				t.Errorf("Idempotency-Key = %q", r.Header.Get("Idempotency-Key"))
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"x"}`)
		}
	}))
	defer srv.Close()
	c, err := simkit.NewClient("testsim", srv.URL, nil, func(status int, code string) error {
		if status == http.StatusUnprocessableEntity && code == "refused" {
			return errRefused
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ ID string }
	if err := c.Do(t.Context(), http.MethodPost, "/ok", "k", map[string]int{"a": 1}, http.StatusCreated, &out); err != nil || out.ID != "x" {
		t.Errorf("Do(/ok) = %v, %+v", err, out)
	}
	if err := c.Do(t.Context(), http.MethodPost, "/refused", "", nil, http.StatusCreated, nil); !errors.Is(err, errRefused) {
		t.Errorf("Do(/refused) = %v, want %v", err, errRefused)
	}
	for _, path := range []string{"/other", "/down"} {
		if err := c.Do(t.Context(), http.MethodPost, path, "", nil, http.StatusCreated, nil); !errors.Is(err, simkit.ErrBadResponse) || errors.Is(err, errRefused) {
			t.Errorf("Do(%s) = %v, want only %v", path, err, simkit.ErrBadResponse)
		}
	}
	if _, err := simkit.NewClient("testsim", "localhost:8090", nil, nil); err == nil {
		t.Error("a base URL without a scheme was accepted")
	}
}
