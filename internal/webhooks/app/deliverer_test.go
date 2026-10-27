package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	merchants "github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
)

func TestFullJitterDrawsUpToTheCeiling(t *testing.T) {
	const ceiling = 30 * time.Second
	var below bool
	for range 1000 {
		d := FullJitter(ceiling)
		if d < 0 || d > ceiling {
			t.Fatalf("FullJitter(%v) = %v", ceiling, d)
		}
		below = below || d < ceiling/2
	}
	if !below {
		t.Error("1000 draws, none in the lower half: not uniform over [0, ceiling]")
	}
}

type fakeQueue struct {
	batch    []events.Delivery
	recorded []string
}

func (q *fakeQueue) Claim(context.Context, time.Time, time.Time, int, int) ([]events.Delivery, error) {
	return q.batch, nil
}

func (q *fakeQueue) Delivered(context.Context, *events.Delivery, int, time.Time) (bool, error) {
	q.recorded = append(q.recorded, "delivered")
	return true, nil
}

func (q *fakeQueue) Reschedule(context.Context, *events.Delivery, int, string, time.Time) (bool, error) {
	q.recorded = append(q.recorded, "rescheduled")
	return true, nil
}

func (q *fakeQueue) Fail(context.Context, *events.Delivery, int, string, time.Time) (bool, error) {
	q.recorded = append(q.recorded, "failed")
	return true, nil
}

type oneEndpoint struct{}

func (oneEndpoint) Endpoint(_ context.Context, m merchants.MerchantID) (merchants.Endpoint, error) {
	return merchants.Endpoint{Merchant: m, URL: "https://example.com/hooks", Version: 1}, nil
}

// hangingSender answers nothing until the post's context ends, and says when it started.
type hangingSender struct{ started chan struct{} }

func (s hangingSender) Send(ctx context.Context, _, _ string, _ []byte) (int, error) {
	close(s.started)
	<-ctx.Done()
	return 0, ctx.Err()
}

// A post cut short by the worker stopping is not the merchant's failure: nothing is
// recorded, the lease brings the event back, and the round ends without an error.
func TestDeliverRecordsNothingForAPostCutByShutdown(t *testing.T) {
	q := &fakeQueue{batch: []events.Delivery{{Event: events.Outgoing{Payload: []byte("{}"), CreatedAt: time.Now()}, Attempt: 1}}}
	sender := hangingSender{started: make(chan struct{})}
	d, err := NewDeliverer(q, oneEndpoint{}, sender, make([]byte, merchants.MinSigningKeyBytes), clock.System{}, FullJitter, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-sender.started
		cancel()
	}()
	if n, err := d.Deliver(ctx, DeliverBatch); n != 1 || err != nil {
		t.Errorf("Deliver() = %d, %v, want 1 and no error", n, err)
	}
	if len(q.recorded) != 0 {
		t.Errorf("recorded %v for a post cut by shutdown", q.recorded)
	}
}
