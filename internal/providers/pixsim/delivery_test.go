package pixsim_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim/server"
)

type delivery struct {
	header string
	body   []byte
}

// webhook records every delivery and answers status.
type webhook struct {
	url string

	mu         sync.Mutex
	status     int
	deliveries []delivery
}

func newWebhook(t *testing.T) *webhook {
	t.Helper()
	h := &webhook{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.deliveries = append(h.deliveries, delivery{r.Header.Get(pixsim.SignatureHeader), body})
		w.WriteHeader(h.status)
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

func (h *webhook) answer(status int) {
	h.mu.Lock()
	h.status = status
	h.mu.Unlock()
}

func (h *webhook) received() []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]delivery(nil), h.deliveries...)
}

func withWebhook(h *webhook) *server.Config {
	cfg := receiver
	cfg.WebhookURL, cfg.WebhookSecret = h.url, secret
	return &cfg
}

func verify(t *testing.T, d delivery, now time.Time) pixsim.Event {
	t.Helper()
	e, err := pixsim.VerifyEvent(secret, d.header, d.body, now)
	if err != nil {
		t.Fatalf("VerifyEvent() error = %v", err)
	}
	return e
}

func TestPayingSendsOneSignedEvent(t *testing.T) {
	h := newWebhook(t)
	s := start(t, withWebhook(h), time.Second)
	ch := s.create(t, "att_1", charge(10150))
	s.clock.Advance(time.Minute)

	paid, err := s.client.Pay(t.Context(), ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := h.received()
	if len(got) != 1 {
		t.Fatalf("%d deliveries after paying, want 1", len(got))
	}
	e := verify(t, got[0], s.clock.Now())
	if e.Type != pixsim.ChargePaid || len(e.ID) != len("evt_")+20 || !e.CreatedAt.Equal(*paid.PaidAt) ||
		e.Charge.ID != ch.ID || e.Charge.IdempotencyKey != "att_1" || e.Charge.Status != pixsim.Paid || e.Charge.BRCode != ch.BRCode {
		t.Errorf("event = %+v, want charge.paid for %+v", e, paid)
	}

	if _, err := s.client.Pay(t.Context(), ch.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.received()); n != 1 {
		t.Errorf("%d deliveries after paying twice; a second pay is no new fact", n)
	}
}

// A redelivery is the same event: same ID and same bytes, signed again at its own time.
func TestRedeliverSendsTheSameEvent(t *testing.T) {
	h := newWebhook(t)
	s := start(t, withWebhook(h), time.Second)
	ch := s.create(t, "att_1", charge(10150))
	if _, err := s.client.Pay(t.Context(), ch.ID); err != nil {
		t.Fatal(err)
	}
	first := verify(t, h.received()[0], s.clock.Now())

	s.clock.Advance(time.Hour)
	if err := s.client.Redeliver(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	got := h.received()
	if len(got) != 2 || !bytes.Equal(got[1].body, got[0].body) || got[1].header == got[0].header {
		t.Fatalf("deliveries %d, same body %v, same signature %v; want 2, true, false",
			len(got), bytes.Equal(got[1].body, got[0].body), got[1].header == got[0].header)
	}
	if again := verify(t, got[1], s.clock.Now()); again.ID != first.ID {
		t.Errorf("redelivered %s, want %s", again.ID, first.ID)
	}
	if _, err := pixsim.VerifyEvent(secret, got[0].header, got[0].body, s.clock.Now()); !errors.Is(err, pixsim.ErrStaleSignature) {
		t.Errorf("first delivery replayed an hour later: error = %v, want %v", err, pixsim.ErrStaleSignature)
	}
	if err := s.client.Redeliver(t.Context(), "evt_unknown"); !errors.Is(err, pixsim.ErrEventNotFound) {
		t.Errorf("Redeliver(unknown) error = %v, want %v", err, pixsim.ErrEventNotFound)
	}
}

// The payment happens whatever the webhook answers; a lost event waits for a
// redelivery.
func TestFailedDeliveryDoesNotUndoThePayment(t *testing.T) {
	h := newWebhook(t)
	h.answer(http.StatusInternalServerError)
	s := start(t, withWebhook(h), time.Second)
	ch := s.create(t, "att_1", charge(10150))

	if paid, err := s.client.Pay(t.Context(), ch.ID); err != nil || paid.Status != pixsim.Paid {
		t.Fatalf("Pay() = %+v, %v; want paid", paid, err)
	}
	id := verify(t, h.received()[0], s.clock.Now()).ID
	if err := s.client.Redeliver(t.Context(), id); !errors.Is(err, pixsim.ErrDeliveryFailed) {
		t.Errorf("Redeliver() to a failing webhook: error = %v, want %v", err, pixsim.ErrDeliveryFailed)
	}
	h.answer(http.StatusNoContent)
	if err := s.client.Redeliver(t.Context(), id); err != nil {
		t.Errorf("Redeliver() once the webhook is back: %v", err)
	}
	if n := len(h.received()); n != 3 {
		t.Errorf("%d deliveries, want 3", n)
	}
}

func TestSameSeedSameEventIDs(t *testing.T) {
	ids := func() [2]string {
		h := newWebhook(t)
		s := start(t, withWebhook(h), time.Second)
		for _, key := range []string{"att_1", "att_2"} {
			if _, err := s.client.Pay(t.Context(), s.create(t, key, charge(10150)).ID); err != nil {
				t.Fatal(err)
			}
		}
		got := h.received()
		return [2]string{verify(t, got[0], t0).ID, verify(t, got[1], t0).ID}
	}
	first, second := ids(), ids()
	if first != second || first[0] == first[1] {
		t.Errorf("event IDs %v then %v; want the same per seed and distinct per charge", first, second)
	}
}

func TestNoWebhookNoEvents(t *testing.T) {
	s := start(t, &receiver, time.Second)
	ch := s.create(t, "att_1", charge(10150))
	if _, err := s.client.Pay(t.Context(), ch.ID); err != nil {
		t.Fatal(err)
	}
	cfg := receiver
	cfg.WebhookURL = "http://localhost:1/hook"
	if _, err := server.New(&cfg, clock.NewFake(t0), slog.New(slog.DiscardHandler)); err == nil {
		t.Error("New() accepted a webhook URL without a secret")
	}
}

// Section 4.3 from the provider's side: the payment happened, so a payer whose request
// gives up must not cut the event on its way to the gateway.
func TestPayerGivingUpDoesNotCutTheDelivery(t *testing.T) {
	release := make(chan struct{})
	cut := make(chan bool, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Reading the whole body lets the server notice a closed connection, which
		// cancels r's context: the sign that the sender gave up on this delivery.
		_, _ = io.ReadAll(r.Body)
		<-release
		select {
		case <-r.Context().Done():
			cut <- true
		case <-time.After(100 * time.Millisecond):
			cut <- false
		}
	}))
	t.Cleanup(hook.Close)
	cfg := receiver
	cfg.WebhookURL, cfg.WebhookSecret = hook.URL, secret
	s := start(t, &cfg, 100*time.Millisecond)
	ch := s.create(t, "att_1", charge(10150))

	if _, err := s.client.Pay(t.Context(), ch.ID); err == nil {
		t.Fatal("Pay() answered while the delivery was held")
	}
	close(release)
	select {
	case wasCut := <-cut:
		if wasCut {
			t.Error("the delivery was cut when the payer gave up")
		}
	case <-time.After(server.DeliveryTimeout):
		t.Fatal("no delivery")
	}
}
