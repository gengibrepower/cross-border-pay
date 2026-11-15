package main

import (
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	eventsadapters "github.com/gengibrepower/cross-border-pay/internal/events/adapters"
	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	paymentsadapters "github.com/gengibrepower/cross-border-pay/internal/payments/adapters"
	paymentsapp "github.com/gengibrepower/cross-border-pay/internal/payments/app"
	payments "github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
)

// cardPaid is the GameStore's US$ 20.00 by card, captured on create: R$ 101.50.
func (c *contract) cardPaid(key string) string {
	c.t.Helper()
	p := cardOf(c.t, c.do(postPayment(c.t, key, rand.Text(), cardBody(cardsim.TokenApproved)), true))
	if p.Status != "paid" {
		c.t.Fatalf("card payment = %+v, want paid", p)
	}
	return p.ID
}

// openChargeback has the cardholder's bank take payer back from the payment's capture
// and lets the worker apply the event. It returns the gateway's chargeback ID and the
// acquirer's.
func (c *contract) openChargeback(paymentID string, payer int64) (id, external string) {
	c.t.Helper()
	cb, err := c.card.OpenChargeback(c.t.Context(), c.attemptOf(paymentID).externalID,
		cardsim.OpenChargeback{Amount: money.New(payer, money.BRL), Reason: "fraudulent"})
	if err != nil {
		c.t.Fatalf("OpenChargeback(%d) error = %v", payer, err)
	}
	c.processInbox()
	var row pgtype.UUID
	if err := c.pool.QueryRow(c.t.Context(), "SELECT id FROM chargebacks WHERE external_id = $1", cb.ID).Scan(&row); err != nil {
		c.t.Fatalf("chargeback %s not stored: %v (inbox: %s)", cb.ID, err, c.inboxErrors())
	}
	return payments.ChargebackID(row.Bytes).String(), cb.ID
}

func (c *contract) inboxErrors() string {
	c.t.Helper()
	rows, err := c.pool.Query(c.t.Context(), "SELECT status || ': ' || coalesce(last_error, '') FROM inbox_events ORDER BY received_at")
	if err != nil {
		c.t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

type chargebackBody struct {
	ID          string    `json:"id"`
	PaymentID   string    `json:"payment_id"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason"`
	PayerAmount moneyBody `json:"payer_amount"`
	Amount      moneyBody `json:"amount"`
	Evidence    *string   `json:"evidence"`
	ClosedAt    *string   `json:"closed_at"`
}

func chargebackOf(t *testing.T, rec *httptest.ResponseRecorder) chargebackBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var cb chargebackBody
	if err := json.Unmarshal(rec.Body.Bytes(), &cb); err != nil {
		t.Fatal(err)
	}
	return cb
}

func (c *contract) contest(key, id string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.do(postChargeback(c.t, key, "/v1/chargebacks/"+id+"/contest", `{"evidence": "delivered: tracking BR123"}`), true)
}

func (c *contract) resolve(external string, outcome cardsim.ChargebackStatus) {
	c.t.Helper()
	if _, err := c.card.ResolveChargeback(c.t.Context(), external, outcome); err != nil {
		c.t.Fatalf("ResolveChargeback(%s) error = %v", outcome, err)
	}
	c.processInbox()
}

// outboxTypes lists the payment's events in the order they were appended, each
// checked against its schema.
func (c *contract) outboxTypes(paymentID string) []string {
	c.t.Helper()
	rows, err := c.pool.Query(c.t.Context(), "SELECT type, payload FROM outbox_events WHERE subject_id = $1 ORDER BY id", paymentID)
	if err != nil {
		c.t.Fatal(err)
	}
	var types []string
	for rows.Next() {
		var (
			t       string
			payload []byte
		)
		if err := rows.Scan(&t, &payload); err != nil {
			c.t.Fatal(err)
		}
		types = append(types, t)
		if strings.HasPrefix(t, "chargeback.") {
			var event any
			if err := json.Unmarshal(payload, &event); err != nil {
				c.t.Fatal(err)
			}
			if err := c.doc.Components.Schemas["ChargebackEvent"].Value.VisitJSON(event); err != nil {
				c.t.Errorf("%s does not match ChargebackEvent: %v\n%s", t, err, payload)
			}
		}
	}
	if err := rows.Err(); err != nil {
		c.t.Fatal(err)
	}
	return types
}

// F8's done criterion, section 4.7: the GameStore's card payment settled, then charged
// back. The bank takes the R$ 101.50 back, the merchant loses the US$ 20.00 it was
// paid for, fee kept, out of a balance already paid out, so it goes negative; the
// next run takes the reserve. Contested and won, the money comes back.
func TestChargebackAfterSettlementIsContestedAndWon(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	s := c.settler()

	id := c.cardPaid(key)
	c.toDate(2026, time.December, 1) // card is D+30
	if n := c.settle(s); n != 1 {
		t.Fatalf("settled %d on day 30, want 1", n)
	}
	c.wantBalance(key, "settled", 0, 0, 194)

	cbID, external := c.openChargeback(id, 10150)
	if status, _ := c.statusOf(id); status != "charged_back" {
		t.Errorf("payment is %s, want charged_back", status)
	}
	cb := chargebackOf(t, c.do(getRequest(t, key, "/v1/chargebacks/"+cbID), true))
	if cb.Status != "needs_response" || cb.PaymentID != id || cb.PayerAmount.Value != 10150 || cb.Amount.Value != 2000 ||
		cb.Reason != "fraudulent" || cb.Evidence != nil || cb.ClosedAt != nil {
		t.Fatalf("chargeback = %+v, want needs_response of R$ 101.50 costing US$ 20.00", cb)
	}
	c.wantBalance(key, "charged back", -2000, 0, 194)
	if got := c.balance("provider_receivables:"); got != 0 {
		t.Errorf("provider receivables = %d, want 0: the bank took the R$ 101.50 back", got)
	}
	if got := c.balance("fx_result:"); got != 0 {
		t.Errorf("fx result = %d, want 0 at the same rate", got)
	}
	c.toDate(2026, time.December, 2)
	c.settle(s)
	c.wantBalance(key, "the reserve covered", -1806, 0, 0)

	cb = chargebackOf(t, c.contest(key, cbID))
	if cb.Status != "under_review" || cb.Evidence == nil || *cb.Evidence != "delivered: tracking BR123" {
		t.Fatalf("contested = %+v, want under_review with the evidence", cb)
	}
	if again := chargebackOf(t, c.contest(key, cbID)); again.Status != "under_review" {
		t.Errorf("contested again = %+v, want it as it was", again)
	}
	c.resolve(external, cardsim.Won)
	cb = chargebackOf(t, c.do(getRequest(t, key, "/v1/chargebacks/"+cbID), true))
	if cb.Status != "won" || cb.ClosedAt == nil {
		t.Fatalf("after the issuer's decision = %+v, want won", cb)
	}
	c.wantBalance(key, "won", 194, 0, 0)
	if got := c.balance("provider_receivables:"); got != 10150 {
		t.Errorf("provider receivables = %d, want the R$ 101.50 back", got)
	}
	if got := c.outboxTypes(id); strings.Join(got, ",") != "payment.paid,chargeback.opened,chargeback.won" {
		t.Errorf("events = %v, want payment.paid, chargeback.opened, chargeback.won", got)
	}
	c.checkBooksBalance()
}

// Accepting gives the money up at once; the acquirer's closed event that follows is
// the same fact, and the chargeback takes no other answer after.
func TestAcceptedChargebackIsLost(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.cardPaid(key)
	cbID, _ := c.openChargeback(id, 10150)

	accept := func() *httptest.ResponseRecorder {
		return c.do(postChargeback(t, key, "/v1/chargebacks/"+cbID+"/accept", ""), true)
	}
	cb := chargebackOf(t, accept())
	if cb.Status != "lost" || cb.ClosedAt == nil {
		t.Fatalf("accepted = %+v, want lost", cb)
	}
	c.processInbox() // the acquirer's chargeback.closed
	if again := chargebackOf(t, accept()); again.Status != "lost" {
		t.Errorf("accepted again = %+v, want lost as it was", again)
	}
	if rec := c.contest(key, cbID); rec.Code != http.StatusConflict || problemCode(t, rec) != "chargeback_closed" {
		t.Errorf("contest after accepting = %d %s, want 409 chargeback_closed", rec.Code, rec.Body)
	}
	// Not available yet (card is D+30): the -60 is the payment's pending net.
	c.wantBalance(key, "lost", 0, -60, 0)
	if got := c.outboxTypes(id); strings.Join(got, ",") != "payment.paid,chargeback.opened,chargeback.lost" {
		t.Errorf("events = %v, want one chargeback.lost", got)
	}
	c.checkBooksBalance()
}

// Decision 7: a refunded payment can still be charged back, and the money leaves twice:
// the merchant refunded it and loses it again, until the dispute, with the refund as
// evidence, is won. Another payment's money is at the acquirer, so the books hold it.
func TestRefundedPaymentChargedBackTakesTheMoneyTwice(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.cardPaid(key)
	c.cardPaid(key)
	refund := refundOf(t, c.do(postRefund(t, key, "/v1/payments/"+id, "r1", usdBody(2000)), true))
	if refund.Status != "succeeded" {
		t.Fatalf("refund = %+v, want succeeded", refund)
	}

	cbID, external := c.openChargeback(id, 10150)
	if status, _ := c.statusOf(id); status != "charged_back" {
		t.Errorf("refunded payment is %s after the chargeback, want charged_back", status)
	}
	// Two payments in, one refunded and charged back: 2 × 1940 - 2000 - 2000.
	if got := c.balance("merchant_payable:"); got != -120 {
		t.Errorf("merchant payable = %d, want -120: the money left twice", got)
	}
	if rec := c.do(postRefund(t, key, "/v1/payments/"+id, "r2", usdBody(1)), true); rec.Code != http.StatusConflict {
		t.Errorf("refund of a charged back payment = %d %s, want 409", rec.Code, rec.Body)
	}
	if rec := c.contest(key, cbID); rec.Code != http.StatusOK {
		t.Fatalf("contest = %d %s", rec.Code, rec.Body)
	}
	c.resolve(external, cardsim.Won)
	if got := c.balance("merchant_payable:"); got != 1880 {
		t.Errorf("merchant payable = %d, want 1880: refunded once, as it should", got)
	}
	c.checkBooksBalance()
}

// Partial chargebacks are priced cumulatively, like refunds: two halves of R$ 101.50
// cost US$ 10.00 each, and the acquirer refuses a third.
func TestPartialChargebacksAddUpToThePayment(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.cardPaid(key)

	first, _ := c.openChargeback(id, 5075)
	second, _ := c.openChargeback(id, 5075)
	for _, cbID := range []string{first, second} {
		if cb := chargebackOf(t, c.do(getRequest(t, key, "/v1/chargebacks/"+cbID), true)); cb.Amount.Value != 1000 {
			t.Errorf("chargeback %s costs %d, want 1000", cbID, cb.Amount.Value)
		}
	}
	if _, err := c.card.OpenChargeback(t.Context(), c.attemptOf(id).externalID,
		cardsim.OpenChargeback{Amount: money.New(1, money.BRL), Reason: "duplicate"}); err == nil {
		t.Error("a third chargeback was opened past the capture")
	}
	if got := c.balance("merchant_payable:"); got != -60 {
		t.Errorf("merchant payable = %d, want -60", got)
	}
	if got := c.outboxTypes(id); strings.Join(got, ",") != "payment.paid,chargeback.opened,chargeback.opened" {
		t.Errorf("events = %v", got)
	}
	c.checkBooksBalance()
}

// Charged back before it was available, the payment's net leaves the pending part of
// the balance, not the available one.
func TestChargebackOfPendingMoneyComesOutOfPending(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.cardPaid(key)
	c.openChargeback(id, 5075)
	c.wantBalance(key, "half charged back", 0, 940, 0)
}

func TestChargebackErrors(t *testing.T) {
	c := newContract(t)
	key, other := c.merchantKey(), c.merchantKey()
	id := c.cardPaid(key)
	cbID, _ := c.openChargeback(id, 10150)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"another merchant's": c.do(getRequest(t, other, "/v1/chargebacks/"+cbID), true),
		"unknown":            c.do(getRequest(t, key, "/v1/chargebacks/cbk_00000000000000000000000000"), true),
		"not an ID":          c.do(getRequest(t, key, "/v1/chargebacks/pay_00000000000000000000000000"), false),
		"another's accept":   c.do(postChargeback(t, other, "/v1/chargebacks/"+cbID+"/accept", ""), true),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s, want 404", name, rec.Code, rec.Body)
		}
	}
	// Too long is outside the contract too, so the request is not checked against it.
	for name, body := range map[string]string{"empty": `{"evidence": "  "}`, "too long": `{"evidence": "` + strings.Repeat("x", 2001) + `"}`} {
		rec := c.do(postChargeback(t, key, "/v1/chargebacks/"+cbID+"/contest", body), false)
		if rec.Code != http.StatusUnprocessableEntity || problemCode(t, rec) != "invalid_evidence" {
			t.Errorf("%s evidence: %d %s, want 422 invalid_evidence", name, rec.Code, rec.Body)
		}
	}
	if err := c.card.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpRespond, Kind: cardsim.FailBefore, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if rec := c.contest(key, cbID); rec.Code != http.StatusServiceUnavailable || problemCode(t, rec) != "provider_unavailable" {
		t.Errorf("contest with the acquirer down: %d %s, want 503", rec.Code, rec.Body)
	}
	if cb := chargebackOf(t, c.do(getRequest(t, key, "/v1/chargebacks/"+cbID), true)); cb.Status != "needs_response" {
		t.Errorf("after the failed contest = %+v, want needs_response", cb)
	}
	if rec := c.contest(key, cbID); rec.Code != http.StatusOK {
		t.Errorf("contest retried = %d %s, want 200", rec.Code, rec.Body)
	}
}

// The acquirer may send the same opening again under another event ID, and a close
// before its opening: neither takes money twice, and the early close waits for it.
func TestChargebackEventsReplayedAndOutOfOrder(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	id := c.cardPaid(key)
	_, external := c.openChargeback(id, 10150)

	cb, err := c.card.GetChargeback(t.Context(), external)
	if err != nil {
		t.Fatal(err)
	}
	c.receiveCardEvent("evt_replayed", cardsim.ChargebackOpened, &cb)
	if _, err := c.card.Accept(t.Context(), external); err != nil {
		t.Fatal(err)
	}
	cb.Status = cardsim.Lost
	c.receiveCardEvent("evt_close_again", cardsim.ChargebackClosed, &cb)
	c.processInbox()
	if got := c.balance("merchant_payable:"); got != -60 {
		t.Errorf("merchant payable = %d, want -60: one chargeback", got)
	}
	if got := c.outboxTypes(id); strings.Join(got, ",") != "payment.paid,chargeback.opened,chargeback.lost" {
		t.Errorf("events = %v", got)
	}
	if errs := c.inboxErrors(); strings.Contains(errs, "failed") {
		t.Errorf("inbox = %s, want every event processed", errs)
	}

	// A close for a chargeback not stored yet is retried, not failed.
	other := c.cardPaid(key)
	early := cardsim.Chargeback{
		ID: "cbk_early", AuthorizationID: "auth", AuthorizationKey: c.attemptOf(other).id.String(),
		Status: cardsim.Won, Amount: money.New(10150, money.BRL), Reason: "fraudulent", CreatedAt: c.clock.Now(),
	}
	c.receiveCardEvent("evt_early", cardsim.ChargebackClosed, &early)
	c.processInbox()
	var status string
	if err := c.pool.QueryRow(t.Context(), "SELECT status FROM inbox_events WHERE external_id = 'evt_early'").Scan(&status); err != nil || status != "pending" {
		t.Errorf("early close is %s (%v), want pending", status, err)
	}
}

// receiveCardEvent stores an acquirer event in the inbox as if its webhook arrived.
func (c *contract) receiveCardEvent(eventID string, kind cardsim.EventType, cb *cardsim.Chargeback) {
	c.t.Helper()
	payload, err := json.Marshal(cardsim.Event{ID: eventID, Type: kind, CreatedAt: c.clock.Now(), Chargeback: *cb})
	if err != nil {
		c.t.Fatal(err)
	}
	in := events.Incoming{Provider: "cardsim", ExternalID: eventID, Type: string(kind), Payload: payload, Signature: "checked by the API"}
	if _, _, err := eventsadapters.NewPostgres(c.pool).Insert(c.t.Context(), &in, c.clock.Now()); err != nil {
		c.t.Fatal(err)
	}
}

// A chargeback of a capture the gateway is still confirming waits for the
// confirmation, then applies.
func TestChargebackBeforeTheCaptureIsConfirmedWaits(t *testing.T) {
	c := newContract(t)
	key := c.merchantKey()
	if err := c.card.InjectFault(t.Context(), cardsim.Fault{Op: cardsim.OpAuthorize, Kind: cardsim.TimeoutAfter, Count: 1}); err != nil {
		t.Fatal(err)
	}
	p := cardOf(t, c.do(postPayment(t, key, "order-123", cardBody(cardsim.TokenApproved)), true))
	if p.Status != "pending" {
		t.Fatalf("payment = %+v, want pending", p)
	}
	early := cardsim.Chargeback{
		ID: "cbk_before", AuthorizationID: "auth", AuthorizationKey: c.attemptOf(p.ID).id.String(),
		Status: cardsim.NeedsResponse, Amount: money.New(10150, money.BRL), Reason: "fraudulent", CreatedAt: c.clock.Now(),
	}
	c.receiveCardEvent("evt_before", cardsim.ChargebackOpened, &early)
	c.processInbox()
	var status string
	if err := c.pool.QueryRow(t.Context(), "SELECT status FROM inbox_events WHERE external_id = 'evt_before'").Scan(&status); err != nil || status != "pending" {
		t.Fatalf("chargeback of a pending payment is %s (%v), want pending", status, err)
	}

	resolver := paymentsapp.NewResolver(paymentsadapters.NewPostgres(c.pool, paymentSinks()), c.deps.providers, c.clock,
		slog.New(slog.DiscardHandler), providerTimeout)
	c.clock.Advance(payments.CheckBase)
	if _, err := resolver.Resolve(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	c.clock.Advance(time.Hour) // past the inbox's retry
	c.processInbox()
	if got, _ := c.statusOf(p.ID); got != "charged_back" {
		t.Errorf("after the confirmation the payment is %s, want charged_back (inbox: %s)", got, c.inboxErrors())
	}
}
