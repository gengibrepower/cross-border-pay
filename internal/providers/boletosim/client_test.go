package boletosim_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/boleto"
	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim/server"
)

// t0 is Friday 13 November 2026, 09:00 in São Paulo.
var t0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

var bank = server.Config{Seed: 42, Bank: "999", Beneficiary: "00012345"}

const payerCPF = "52998224725"

type sim struct {
	clock  *clock.Fake
	client *boletosim.Client
}

func brazil(t testing.TB) calendar.Calendar {
	t.Helper()
	c, err := calendar.Brazil()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func start(t *testing.T, cfg *server.Config) *sim {
	t.Helper()
	c := clock.NewFake(t0)
	s, err := server.New(cfg, c, brazil(t), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := boletosim.NewClient(srv.URL, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return &sim{clock: c, client: client}
}

func day(t *testing.T, s string) calendar.Date {
	t.Helper()
	d, err := calendar.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func request(t *testing.T, amount int64, due string) boletosim.CreateBoleto {
	t.Helper()
	return boletosim.CreateBoleto{
		Amount: money.New(amount, money.BRL), DueDate: day(t, due),
		Payer: boletosim.Payer{Name: "Maria Silva", Document: payerCPF, Email: "maria@example.com"},
	}
}

func (s *sim) create(t *testing.T, key string, req boletosim.CreateBoleto) boletosim.Boleto { //nolint:gocritic // hugeParam: a test helper, called with literals
	t.Helper()
	b, err := s.client.Create(t.Context(), key, &req)
	if err != nil {
		t.Fatalf("Create(%q) error = %v", key, err)
	}
	return b
}

func TestCreateRegistersAValidBoleto(t *testing.T) {
	s := start(t, &bank)
	b := s.create(t, "att_1", request(t, 52_634, "2026-11-16"))

	if b.Status != boletosim.Registered || !strings.HasPrefix(b.ID, "bol_") || b.IdempotencyKey != "att_1" ||
		b.Amount != money.New(52_634, money.BRL) || b.DueDate != day(t, "2026-11-16") || !b.CreatedAt.Equal(t0) || b.PaidAt != nil {
		t.Fatalf("Create() = %+v", b)
	}
	parsed, err := boleto.Parse(b.Barcode, day(t, "2026-11-13"))
	if err != nil {
		t.Fatalf("barcode %q: %v", b.Barcode, err)
	}
	want := boleto.Boleto{Bank: "999", Due: b.DueDate, Amount: b.Amount, FreeField: "00012345" + b.NossoNumero}
	if parsed != want {
		t.Errorf("barcode says %+v, want %+v", parsed, want)
	}
	if back, err := boleto.BarcodeFromLine(b.DigitableLine); err != nil || back != b.Barcode {
		t.Errorf("digitable line %q gives %q, %v", b.DigitableLine, back, err)
	}

	again := s.create(t, "att_1", request(t, 52_634, "2026-11-16"))
	if again != b {
		t.Errorf("the same request again = %+v, want %+v", again, b)
	}
	got, err := s.client.Get(t.Context(), "att_1")
	if err != nil || got != b {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if _, err := s.client.Create(t.Context(), "att_1", ptr(request(t, 52_635, "2026-11-16"))); !errors.Is(err, boletosim.ErrKeyReused) {
		t.Errorf("another amount with the same key: error = %v, want ErrKeyReused", err)
	}
	if _, err := s.client.Get(t.Context(), "att_none"); !errors.Is(err, boletosim.ErrNotFound) {
		t.Errorf("Get of an unknown key: error = %v, want ErrNotFound", err)
	}
}

func TestSameSeedSameBoletos(t *testing.T) {
	a, b := start(t, &bank), start(t, &bank)
	other := bank
	other.Seed = 43
	c := start(t, &other)
	req := request(t, 100, "2026-11-16")
	first, second, third := a.create(t, "att_1", req), b.create(t, "att_1", req), c.create(t, "att_1", req)
	if first.ID != second.ID || first.Barcode != second.Barcode {
		t.Errorf("same seed: %s %s and %s %s", first.ID, first.Barcode, second.ID, second.Barcode)
	}
	if first.ID == third.ID || first.Barcode == third.Barcode {
		t.Errorf("another seed gave the same boleto %s", first.ID)
	}
}

func TestCreateRefusesBadRequests(t *testing.T) {
	s := start(t, &bank)
	edits := map[string]func(*boletosim.CreateBoleto){
		"dollars":        func(r *boletosim.CreateBoleto) { r.Amount = money.New(100, money.USD) },
		"zero":           func(r *boletosim.CreateBoleto) { r.Amount = money.New(0, money.BRL) },
		"due yesterday":  func(r *boletosim.CreateBoleto) { r.DueDate = day(t, "2026-11-12") },
		"no name":        func(r *boletosim.CreateBoleto) { r.Payer.Name = "  " },
		"invalid CPF":    func(r *boletosim.CreateBoleto) { r.Payer.Document = "52998224726" },
		"no CPF":         func(r *boletosim.CreateBoleto) { r.Payer.Document = "" },
		"invalid e-mail": func(r *boletosim.CreateBoleto) { r.Payer.Email = "maria" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			req := request(t, 100, "2026-11-16")
			edit(&req)
			_, err := s.client.Create(t.Context(), "att_"+name, &req)
			if !errors.Is(err, boletosim.ErrBadResponse) || !strings.Contains(err.Error(), "invalid_request") {
				t.Errorf("error = %v, want 400 invalid_request", err)
			}
		})
	}
	// Due today is fine: the payer has until the end of it.
	s.create(t, "att_today", request(t, 100, "2026-11-13"))
}

func TestReceivedUntilTheFirstBankingDayOnOrAfterTheDueDate(t *testing.T) {
	s := start(t, &bank)
	// Due on Saturday 14 November: received on Monday 16, refused on Tuesday 17.
	saturday := s.create(t, "att_sat", request(t, 100, "2026-11-14"))
	other := s.create(t, "att_other", request(t, 100, "2026-11-14"))

	s.clock.Advance(3*24*time.Hour + 14*time.Hour) // Monday 16, 23:00 in São Paulo
	paid, err := s.client.Pay(t.Context(), saturday.ID)
	if err != nil || paid.Status != boletosim.Paid || paid.PaidAt == nil || !paid.PaidAt.Equal(s.clock.Now()) {
		t.Fatalf("Pay on Monday = %+v, %v", paid, err)
	}
	if again, err := s.client.Pay(t.Context(), saturday.ID); err != nil || !reflect.DeepEqual(again, paid) {
		t.Errorf("paying again = %+v, %v, want it as it was", again, err)
	}

	s.clock.Advance(time.Hour) // Tuesday 17, 00:00 in São Paulo
	if _, err := s.client.Pay(t.Context(), other.ID); !errors.Is(err, boletosim.ErrOverdue) {
		t.Errorf("Pay on Tuesday error = %v, want ErrOverdue (decision 53)", err)
	}
	got, err := s.client.Get(t.Context(), "att_other")
	if err != nil || got.Status != boletosim.Expired {
		t.Errorf("Get on Tuesday = %+v, %v, want expired", got, err)
	}
	if got, err := s.client.Get(t.Context(), "att_sat"); err != nil || got.Status != boletosim.Paid {
		t.Errorf("a paid boleto after its day = %+v, %v, want paid", got, err)
	}
}

func TestHolidayMovesTheLastDay(t *testing.T) {
	s := start(t, &bank)
	// Due on Friday 20 November, Consciência Negra: received on Monday 23.
	b := s.create(t, "att_1", request(t, 100, "2026-11-20"))
	s.clock.Advance(10*24*time.Hour + 2*time.Hour) // Monday 23, 11:00 in São Paulo
	if paid, err := s.client.Pay(t.Context(), b.ID); err != nil || paid.Status != boletosim.Paid {
		t.Fatalf("Pay on the next banking day = %+v, %v", paid, err)
	}
}

func TestRejectAndFaults(t *testing.T) {
	s := start(t, &bank)
	if err := s.client.InjectFault(t.Context(), boletosim.Fault{Op: boletosim.OpCreate, Kind: boletosim.Reject, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.Create(t.Context(), "att_1", ptr(request(t, 100, "2026-11-16"))); !errors.Is(err, boletosim.ErrRejected) {
		t.Fatalf("rejected create error = %v, want ErrRejected", err)
	}
	got, err := s.client.Get(t.Context(), "att_1")
	if err != nil || got.Status != boletosim.Rejected || got.Barcode != "" || got.DigitableLine != "" {
		t.Errorf("Get of the rejected boleto = %+v, %v", got, err)
	}
	if _, err := s.client.Pay(t.Context(), got.ID); !errors.Is(err, boletosim.ErrNotPayable) {
		t.Errorf("paying a rejected boleto: error = %v, want ErrNotPayable", err)
	}

	if err := s.client.InjectFault(t.Context(), boletosim.Fault{Op: boletosim.OpCreate, Kind: boletosim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.Create(t.Context(), "att_2", ptr(request(t, 100, "2026-11-16"))); !errors.Is(err, boletosim.ErrBadResponse) {
		t.Errorf("failed create error = %v, want ErrBadResponse", err)
	}
	if _, err := s.client.Get(t.Context(), "att_2"); !errors.Is(err, boletosim.ErrNotFound) {
		t.Errorf("a create that failed before acting left a boleto: %v", err)
	}
}

func TestRefundsNeverExceedWhatWasPaid(t *testing.T) {
	s := start(t, &bank)
	b := s.create(t, "att_1", request(t, 1000, "2026-11-16"))
	if _, err := s.client.Refund(t.Context(), "rfd_0", b.ID, boletosim.CreateRefund{Amount: money.New(100, money.BRL)}); !errors.Is(err, boletosim.ErrRefundRejected) {
		t.Errorf("refund of an unpaid boleto: error = %v, want ErrRefundRejected", err)
	}
	if _, err := s.client.Pay(t.Context(), b.ID); err != nil {
		t.Fatal(err)
	}
	rf, err := s.client.Refund(t.Context(), "rfd_1", b.ID, boletosim.CreateRefund{Amount: money.New(600, money.BRL)})
	if err != nil || rf.Status != boletosim.RefundCompleted || rf.BoletoID != b.ID {
		t.Fatalf("Refund = %+v, %v", rf, err)
	}
	if again, err := s.client.Refund(t.Context(), "rfd_1", b.ID, boletosim.CreateRefund{Amount: money.New(600, money.BRL)}); err != nil || again != rf {
		t.Errorf("the same refund again = %+v, %v", again, err)
	}
	if _, err := s.client.Refund(t.Context(), "rfd_2", b.ID, boletosim.CreateRefund{Amount: money.New(401, money.BRL)}); !errors.Is(err, boletosim.ErrRefundRejected) {
		t.Errorf("refund past what is left: error = %v, want ErrRefundRejected", err)
	}
	if got, err := s.client.GetRefund(t.Context(), "rfd_2"); err != nil || got.Status != boletosim.RefundRejected {
		t.Errorf("GetRefund of the rejected one = %+v, %v", got, err)
	}
	if _, err := s.client.GetRefund(t.Context(), "rfd_none"); !errors.Is(err, boletosim.ErrRefundNotFound) {
		t.Errorf("GetRefund of an unknown key: error = %v, want ErrRefundNotFound", err)
	}
	if _, err := s.client.Refund(t.Context(), "rfd_3", "bol_none", boletosim.CreateRefund{Amount: money.New(1, money.BRL)}); !errors.Is(err, boletosim.ErrNotFound) {
		t.Errorf("refund of an unknown boleto: error = %v, want ErrNotFound", err)
	}
}

type receiver struct {
	mu      sync.Mutex
	headers []string
	bodies  [][]byte
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.headers = append(r.headers, req.Header.Get(boletosim.SignatureHeader))
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func TestPaymentSendsOneSignedEvent(t *testing.T) {
	rec := &receiver{}
	hook := httptest.NewServer(rec)
	t.Cleanup(hook.Close)
	cfg := bank
	cfg.WebhookURL, cfg.WebhookSecret = hook.URL, []byte("secret")
	s := start(t, &cfg)

	b := s.create(t, "att_1", request(t, 100, "2026-11-16"))
	paid, err := s.client.Pay(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.client.Pay(t.Context(), b.ID); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) != 1 {
		t.Fatalf("%d events for one payment, want 1", len(rec.bodies))
	}
	e, err := boletosim.VerifyEvent([]byte("secret"), rec.headers[0], rec.bodies[0], s.clock.Now())
	if err != nil {
		t.Fatalf("VerifyEvent: %v", err)
	}
	if e.Type != boletosim.BoletoPaid || !reflect.DeepEqual(e.Boleto, paid) || !strings.HasPrefix(e.ID, "evt_") {
		t.Errorf("event = %+v, want boleto.paid with %+v", e, paid)
	}
	if strings.Contains(string(rec.bodies[0]), payerCPF) || strings.Contains(string(rec.bodies[0]), "Maria") {
		t.Errorf("the event carries the payer: %s", rec.bodies[0])
	}
	if _, err := boletosim.VerifyEvent([]byte("other"), rec.headers[0], rec.bodies[0], s.clock.Now()); err == nil {
		t.Error("an event signed with another secret verified")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.bodies[0], &raw); err != nil || raw["boleto"] == nil {
		t.Errorf("event body %s", rec.bodies[0])
	}
}

func TestNewRefusesABadAccount(t *testing.T) {
	for name, cfg := range map[string]server.Config{
		"bank":        {Bank: "99", Beneficiary: "00012345"},
		"beneficiary": {Bank: "999", Beneficiary: "123"},
		"letters":     {Bank: "999", Beneficiary: "0001234x"},
		"no secret":   {Bank: "999", Beneficiary: "00012345", WebhookURL: "http://localhost"},
	} {
		if _, err := server.New(&cfg, clock.NewFake(t0), brazil(t), slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func ptr[T any](v T) *T { return &v }
