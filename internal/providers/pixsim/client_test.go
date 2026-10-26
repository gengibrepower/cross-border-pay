package pixsim_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/brcode"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim/server"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

var receiver = server.Config{
	Seed:         42,
	PixKey:       "123e4567-e12b-12d1-a456-426655440000",
	MerchantName: "CROSS BORDER PAY",
	MerchantCity: "SAO PAULO",
}

type sim struct {
	clock  *clock.Fake
	client *pixsim.Client
	url    string
}

func start(t *testing.T, cfg *server.Config, timeout time.Duration) *sim {
	t.Helper()
	c := clock.NewFake(t0)
	s, err := server.New(cfg, c, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := pixsim.NewClient(srv.URL, &http.Client{Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return &sim{clock: c, client: client, url: srv.URL}
}

func charge(amount int64) pixsim.CreateCharge {
	return pixsim.CreateCharge{Amount: money.New(amount, money.BRL), ExpiresAt: t0.Add(30 * time.Minute)}
}

func (s *sim) create(t *testing.T, key string, req pixsim.CreateCharge) pixsim.Charge {
	t.Helper()
	ch, err := s.client.Create(t.Context(), key, req)
	if err != nil {
		t.Fatalf("Create(%q) error = %v", key, err)
	}
	return ch
}

func (s *sim) fault(t *testing.T, f pixsim.Fault) {
	t.Helper()
	if err := s.client.InjectFault(t.Context(), f); err != nil {
		t.Fatal(err)
	}
}

func TestCreateReturnsAPayableBRCode(t *testing.T) {
	s := start(t, &receiver, time.Second)
	ch := s.create(t, "att_1", charge(10150))

	if ch.Status != pixsim.Active || !strings.HasPrefix(ch.ID, "chg_") || ch.IdempotencyKey != "att_1" ||
		ch.Amount != money.New(10150, money.BRL) || !ch.CreatedAt.Equal(t0) || ch.PaidAt != nil {
		t.Fatalf("Create() = %+v", ch)
	}
	code, err := brcode.Parse(ch.BRCode)
	if err != nil {
		t.Fatalf("brcode.Parse(%q) error = %v", ch.BRCode, err)
	}
	want := brcode.Code{
		Key: receiver.PixKey, Amount: ch.Amount, TxID: ch.TxID,
		MerchantName: receiver.MerchantName, MerchantCity: receiver.MerchantCity,
	}
	if code != want || len(ch.TxID) != brcode.MaxTxID {
		t.Errorf("BR Code = %+v, want %+v with a %d-character txid", code, want, brcode.MaxTxID)
	}
}

func TestCreateIsIdempotentPerKey(t *testing.T) {
	s := start(t, &receiver, time.Second)
	first := s.create(t, "att_1", charge(10150))
	again := s.create(t, "att_1", charge(10150))
	if again != first {
		t.Errorf("repeated Create() = %+v, want %+v", again, first)
	}
	if other := s.create(t, "att_2", charge(10150)); other.ID == first.ID || other.TxID == first.TxID {
		t.Errorf("two keys share an ID or txid: %+v and %+v", first, other)
	}
	if _, err := s.client.Create(t.Context(), "att_1", charge(999)); !errors.Is(err, pixsim.ErrKeyReused) {
		t.Errorf("Create() with another amount error = %v, want %v", err, pixsim.ErrKeyReused)
	}
	got, err := s.client.Get(t.Context(), "att_1")
	if err != nil || got != first {
		t.Errorf("Get() = %+v, %v; want %+v", got, err, first)
	}
	if _, err := s.client.Get(t.Context(), "att_unknown"); !errors.Is(err, pixsim.ErrNotFound) {
		t.Errorf("Get(unknown) error = %v, want %v", err, pixsim.ErrNotFound)
	}
}

func TestPay(t *testing.T) {
	s := start(t, &receiver, time.Second)
	ch := s.create(t, "att_1", charge(10150))
	s.clock.Advance(time.Hour) // past expiry: a static code is still payable (decision 8)

	paid, err := s.client.Pay(t.Context(), ch.ID)
	if err != nil || paid.Status != pixsim.Paid || paid.PaidAt == nil || !paid.PaidAt.Equal(s.clock.Now()) {
		t.Fatalf("Pay() = %+v, %v; want paid now", paid, err)
	}
	again, err := s.client.Pay(t.Context(), ch.ID)
	if err != nil || !again.PaidAt.Equal(*paid.PaidAt) {
		t.Errorf("second Pay() = %+v, %v; want the first payment", again, err)
	}
	if got, _ := s.client.Get(t.Context(), "att_1"); got.Status != pixsim.Paid {
		t.Errorf("Get() status = %s, want paid", got.Status)
	}
	if _, err := s.client.Pay(t.Context(), "chg_unknown"); !errors.Is(err, pixsim.ErrNotFound) {
		t.Errorf("Pay(unknown) error = %v, want %v", err, pixsim.ErrNotFound)
	}
}

func TestRejectFault(t *testing.T) {
	s := start(t, &receiver, time.Second)
	s.fault(t, pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.Reject, Count: 1})

	if _, err := s.client.Create(t.Context(), "att_1", charge(10150)); !errors.Is(err, pixsim.ErrRejected) {
		t.Fatalf("Create() error = %v, want %v", err, pixsim.ErrRejected)
	}
	got, err := s.client.Get(t.Context(), "att_1")
	if err != nil || got.Status != pixsim.Rejected || got.BRCode != "" {
		t.Errorf("Get() = %+v, %v; want a rejected charge without BR Code", got, err)
	}
	if _, err := s.client.Create(t.Context(), "att_1", charge(10150)); !errors.Is(err, pixsim.ErrRejected) {
		t.Errorf("retried Create() error = %v, want the stored rejection", err)
	}
	if _, err := s.client.Pay(t.Context(), got.ID); !errors.Is(err, pixsim.ErrNotPayable) {
		t.Errorf("Pay(rejected) error = %v, want %v", err, pixsim.ErrNotPayable)
	}
	if ch := s.create(t, "att_2", charge(10150)); ch.Status != pixsim.Active {
		t.Errorf("Create() after the fault was used = %s, want active", ch.Status)
	}
}

func TestFailBeforeFaultDoesNothing(t *testing.T) {
	s := start(t, &receiver, time.Second)
	s.fault(t, pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.FailBefore, Count: 1})

	_, err := s.client.Create(t.Context(), "att_1", charge(10150))
	if !errors.Is(err, pixsim.ErrBadResponse) {
		t.Fatalf("Create() error = %v, want %v", err, pixsim.ErrBadResponse)
	}
	if _, err := s.client.Get(t.Context(), "att_1"); !errors.Is(err, pixsim.ErrNotFound) {
		t.Errorf("Get() error = %v, want %v: nothing was created", err, pixsim.ErrNotFound)
	}
	if ch := s.create(t, "att_1", charge(10150)); ch.Status != pixsim.Active {
		t.Errorf("retried Create() = %s, want active", ch.Status)
	}
}

// The case section 4.3 is about: the provider created the charge, the caller saw only
// a timeout. Looking it up by key finds it, and retrying returns that same charge.
func TestTimeoutAfterFaultCreatesTheCharge(t *testing.T) {
	s := start(t, &receiver, 200*time.Millisecond)
	s.fault(t, pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.TimeoutAfter, Count: 1})

	_, err := s.client.Create(t.Context(), "att_1", charge(10150))
	if err == nil || errors.Is(err, pixsim.ErrRejected) {
		t.Fatalf("Create() error = %v, want a timeout", err)
	}
	found, err := s.client.Get(t.Context(), "att_1")
	if err != nil || found.Status != pixsim.Active {
		t.Fatalf("Get() = %+v, %v; want the active charge", found, err)
	}
	if retried := s.create(t, "att_1", charge(10150)); retried != found {
		t.Errorf("retried Create() = %+v, want %+v", retried, found)
	}
}

func TestDelayFault(t *testing.T) {
	s := start(t, &receiver, time.Second)
	s.fault(t, pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.Delay, Count: 1, DelayMS: 50})
	begin := time.Now()
	if ch := s.create(t, "att_1", charge(10150)); ch.Status != pixsim.Active || time.Since(begin) < 50*time.Millisecond {
		t.Errorf("delayed Create() = %s after %v, want active after 50ms", ch.Status, time.Since(begin))
	}

	impatient := start(t, &receiver, 100*time.Millisecond)
	impatient.fault(t, pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.Delay, Count: 1, DelayMS: 5000})
	if _, err := impatient.client.Create(t.Context(), "att_1", charge(10150)); err == nil {
		t.Fatal("Create() past the client timeout succeeded")
	}
	if _, err := impatient.client.Get(t.Context(), "att_1"); !errors.Is(err, pixsim.ErrNotFound) {
		t.Errorf("Get() error = %v, want %v: the delay comes before the work", err, pixsim.ErrNotFound)
	}
}

func TestGetFaults(t *testing.T) {
	s := start(t, &receiver, 200*time.Millisecond)
	s.create(t, "att_1", charge(10150))
	s.fault(t, pixsim.Fault{Op: pixsim.OpGet, Kind: pixsim.FailBefore, Count: 1})
	s.fault(t, pixsim.Fault{Op: pixsim.OpGet, Kind: pixsim.TimeoutAfter, Count: 1})

	if _, err := s.client.Get(t.Context(), "att_1"); !errors.Is(err, pixsim.ErrBadResponse) {
		t.Errorf("first Get() error = %v, want %v", err, pixsim.ErrBadResponse)
	}
	if _, err := s.client.Get(t.Context(), "att_1"); err == nil {
		t.Error("second Get() succeeded, want a timeout")
	}
	if _, err := s.client.Get(t.Context(), "att_1"); err != nil {
		t.Errorf("third Get() error = %v, want the faults used up", err)
	}
}

func TestFaultsAreUsedInOrderPerOp(t *testing.T) {
	s := start(t, &receiver, time.Second)
	s.fault(t, pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.Reject, Count: 2})
	s.fault(t, pixsim.Fault{Op: pixsim.OpGet, Kind: pixsim.FailBefore, Count: 1})
	s.fault(t, pixsim.Fault{Op: pixsim.OpCreate, Kind: pixsim.FailBefore, Count: 1})

	for i, want := range []error{pixsim.ErrRejected, pixsim.ErrRejected, pixsim.ErrBadResponse, nil} {
		if _, err := s.client.Create(t.Context(), fmt.Sprint("att_", i), charge(100)); !errors.Is(err, want) {
			t.Errorf("Create #%d error = %v, want %v", i, err, want)
		}
	}
}

func TestInvalidFaultsAreRefused(t *testing.T) {
	s := start(t, &receiver, time.Second)
	for _, f := range []pixsim.Fault{
		{Op: pixsim.OpGet, Kind: pixsim.Reject, Count: 1},
		{Op: "pay", Kind: pixsim.FailBefore, Count: 1},
		{Op: pixsim.OpCreate, Kind: "explode", Count: 1},
		{Op: pixsim.OpCreate, Kind: pixsim.FailBefore, Count: 0},
		{Op: pixsim.OpCreate, Kind: pixsim.Delay, Count: 1},
		{Op: pixsim.OpCreate, Kind: pixsim.Delay, Count: 1, DelayMS: 60_001},
		{Op: pixsim.OpCreate, Kind: pixsim.FailBefore, Count: 1, DelayMS: 10},
	} {
		if err := s.client.InjectFault(t.Context(), f); !errors.Is(err, pixsim.ErrBadResponse) {
			t.Errorf("InjectFault(%+v) error = %v, want a 400", f, err)
		}
	}
}

// Test 5 of the scope, at the simulator's scale: the same seed and keys give the same
// charges, whatever order the requests arrive in.
func TestSameSeedSameCharges(t *testing.T) {
	const n = 50
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("att_%02d", i)
	}

	inOrder := start(t, &receiver, 5*time.Second)
	want := map[string]pixsim.Charge{}
	for _, k := range keys {
		want[k] = inOrder.create(t, k, charge(1000))
	}

	concurrent := start(t, &receiver, 5*time.Second)
	got := make([]pixsim.Charge, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := n - 1; i >= 0; i-- {
		wg.Go(func() { got[i], errs[i] = concurrent.client.Create(t.Context(), keys[i], charge(1000)) })
	}
	wg.Wait()
	for i, k := range keys {
		if errs[i] != nil || got[i] != want[k] {
			t.Errorf("key %s: %+v, %v; want %+v", k, got[i], errs[i], want[k])
		}
	}

	otherSeed := receiver
	otherSeed.Seed++
	if ch := start(t, &otherSeed, time.Second).create(t, keys[0], charge(1000)); ch.ID == want[keys[0]].ID {
		t.Errorf("seeds %d and %d gave the same ID %s", receiver.Seed, otherSeed.Seed, ch.ID)
	}
}

func TestNewClientRejectsRelativeURL(t *testing.T) {
	for _, u := range []string{"", "localhost:8090", "/v1", "ftp://sim"} {
		if _, err := pixsim.NewClient(u, nil); err == nil {
			t.Errorf("NewClient(%q) succeeded", u)
		}
	}
}

func TestServerRefusesBadRequests(t *testing.T) {
	s := start(t, &receiver, time.Second)
	post := func(key, body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.url+"/v1/charges", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	valid := `{"amount":{"value":100,"currency":"BRL"},"expires_at":"2026-11-01T12:30:00Z"}`
	for name, tc := range map[string]struct{ key, body string }{
		"no key":          {"", valid},
		"key too long":    {strings.Repeat("k", 256), valid},
		"decimal amount":  {"k", `{"amount":{"value":1.5,"currency":"BRL"},"expires_at":"2026-11-01T12:30:00Z"}`},
		"USD":             {"k", `{"amount":{"value":100,"currency":"USD"},"expires_at":"2026-11-01T12:30:00Z"}`},
		"zero amount":     {"k", `{"amount":{"value":0,"currency":"BRL"},"expires_at":"2026-11-01T12:30:00Z"}`},
		"no expiry":       {"k", `{"amount":{"value":100,"currency":"BRL"}}`},
		"unknown field":   {"k", `{"amount":{"value":100,"currency":"BRL"},"expires_at":"2026-11-01T12:30:00Z","x":1}`},
		"amount too long": {"k", `{"amount":{"value":10000000000000,"currency":"BRL"},"expires_at":"2026-11-01T12:30:00Z"}`},
	} {
		if got := post(tc.key, tc.body); got != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, got)
		}
	}
	if got := post("k", valid); got != http.StatusCreated {
		t.Errorf("valid request: status = %d, want 201", got)
	}
}

func TestNewRejectsAnInvalidReceiver(t *testing.T) {
	bad := receiver
	bad.MerchantCity = "SAO PAULO DO OESTE"
	if _, err := server.New(&bad, clock.NewFake(t0), slog.New(slog.DiscardHandler)); !errors.Is(err, brcode.ErrFieldTooLong) {
		t.Errorf("New() error = %v, want %v", err, brcode.ErrFieldTooLong)
	}
}
