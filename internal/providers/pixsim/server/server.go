// Package server is the PIX simulator: a provider that keeps its charges in memory,
// derives every random choice from a seed and fails on command.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/gengibrepower/cross-border-pay/internal/brcode"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

// HangLimit bounds a TimeoutAfter fault, in case the caller never gives up.
const HangLimit = simkit.HangLimit

// DeliveryTimeout bounds one webhook delivery.
const DeliveryTimeout = simkit.DeliveryTimeout

const idLength = 20

type Config struct {
	Seed uint64
	// PixKey, MerchantName and MerchantCity are the receiver written into every BR
	// Code: the gateway's own collection account.
	PixKey       string
	MerchantName string
	MerchantCity string
	// WebhookURL receives an event, signed with WebhookSecret, for every fact the
	// gateway must learn about. Empty means no events are sent.
	WebhookURL    string
	WebhookSecret []byte
}

type Clock = simkit.Clock

type record struct {
	req    pixsim.CreateCharge
	charge pixsim.Charge
	// refunded is what completed refunds returned of the charge, in minor units.
	refunded int64
}

type refundRecord struct {
	req    pixsim.CreateRefund
	refund pixsim.Refund
}

type Simulator struct {
	cfg     Config
	seed    simkit.Seed
	clock   Clock
	logger  *slog.Logger
	faults  *simkit.Faults
	webhook *simkit.Webhook

	mu      sync.Mutex
	byKey   map[string]*record
	keyByID map[string]string
	refunds map[string]*refundRecord
}

func New(cfg *Config, clock Clock, logger *slog.Logger) (*Simulator, error) {
	probe := brcode.Code{Key: cfg.PixKey, MerchantName: cfg.MerchantName, MerchantCity: cfg.MerchantCity}
	if _, err := brcode.Encode(&probe); err != nil {
		return nil, fmt.Errorf("pixsim: receiver: %w", err)
	}
	webhook, err := simkit.NewWebhook(cfg.WebhookURL, cfg.WebhookSecret, pixsim.SignatureHeader, clock)
	if err != nil {
		return nil, fmt.Errorf("pixsim: %w", err)
	}
	return &Simulator{
		cfg: *cfg, seed: simkit.Seed(cfg.Seed), clock: clock, logger: logger,
		faults: simkit.NewFaults(pixsim.Ops, pixsim.Rejectable), webhook: webhook,
		byKey: map[string]*record{}, keyByID: map[string]string{}, refunds: map[string]*refundRecord{},
	}, nil
}

func (s *Simulator) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/charges", s.create)
	mux.HandleFunc("GET /v1/charges/by-key/{key}", s.get)
	mux.HandleFunc("POST /v1/charges/{id}/refunds", s.refund)
	mux.HandleFunc("GET /v1/refunds/by-key/{key}", s.getRefund)
	mux.HandleFunc("POST /control/charges/{id}/pay", s.pay)
	mux.HandleFunc("POST /control/faults", s.faults.InjectFault)
	mux.HandleFunc("POST /control/events/{id}/deliver", s.webhook.Redeliver)
}

func (s *Simulator) create(w http.ResponseWriter, r *http.Request) {
	key, ok := simkit.IdempotencyKey(w, r)
	if !ok {
		return
	}
	var req pixsim.CreateCharge
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if err := validateCharge(&req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}

	fault := s.faults.Next(pixsim.OpCreate)
	if !simkit.Before(w, r, &fault) {
		return
	}
	ch, err := s.store(key, &req, fault.Kind == pixsim.Reject)
	simkit.After(r, &fault)
	switch {
	case errors.Is(err, pixsim.ErrKeyReused):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, pixsim.CodeKeyReused, err.Error()))
	case err != nil:
		simkit.Invalid(w, r, err.Error())
	case ch.Status == pixsim.Rejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, pixsim.CodeRejected, "the charge was refused"))
	default:
		simkit.WriteJSON(w, http.StatusCreated, ch)
	}
}

func (s *Simulator) get(w http.ResponseWriter, r *http.Request) {
	fault := s.faults.Next(pixsim.OpGet)
	if !simkit.Before(w, r, &fault) {
		return
	}
	simkit.After(r, &fault)
	s.mu.Lock()
	rec, ok := s.byKey[r.PathValue("key")]
	var ch pixsim.Charge
	if ok {
		ch = rec.charge
	}
	s.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, pixsim.CodeNotFound, "no charge with this idempotency key"))
		return
	}
	simkit.WriteJSON(w, http.StatusOK, ch)
}

func (s *Simulator) refund(w http.ResponseWriter, r *http.Request) {
	key, ok := simkit.IdempotencyKey(w, r)
	if !ok {
		return
	}
	var req pixsim.CreateRefund
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if req.Amount.Amount() <= 0 {
		simkit.Invalid(w, r, fmt.Sprintf("amount must be positive, got %s", req.Amount))
		return
	}

	fault := s.faults.Next(pixsim.OpRefund)
	if !simkit.Before(w, r, &fault) {
		return
	}
	rf, err := s.storeRefund(key, r.PathValue("id"), &req, fault.Kind == pixsim.Reject)
	simkit.After(r, &fault)
	switch {
	case errors.Is(err, pixsim.ErrKeyReused):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, pixsim.CodeKeyReused, err.Error()))
	case errors.Is(err, pixsim.ErrNotFound):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, pixsim.CodeNotFound, "no charge with this ID"))
	case err != nil:
		simkit.Invalid(w, r, err.Error())
	case rf.Status == pixsim.RefundRejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, pixsim.CodeRefundRejected, rf.Reason))
	default:
		simkit.WriteJSON(w, http.StatusCreated, rf)
	}
}

func (s *Simulator) getRefund(w http.ResponseWriter, r *http.Request) {
	fault := s.faults.Next(pixsim.OpGetRefund)
	if !simkit.Before(w, r, &fault) {
		return
	}
	simkit.After(r, &fault)
	s.mu.Lock()
	rec, ok := s.refunds[r.PathValue("key")]
	var rf pixsim.Refund
	if ok {
		rf = rec.refund
	}
	s.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, pixsim.CodeRefundNotFound, "no refund with this idempotency key"))
		return
	}
	simkit.WriteJSON(w, http.StatusOK, rf)
}

// storeRefund decides the refund once per key and keeps it, rejected too, so a caller
// that lost the answer finds the same decision with GetRefund. The simulator holds
// its own line on invariant 8: it never returns more than the charge took in.
func (s *Simulator) storeRefund(key, chargeID string, req *pixsim.CreateRefund, reject bool) (pixsim.Refund, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.refunds[key]; ok {
		if rec.refund.ChargeID != chargeID || rec.req.Amount != req.Amount {
			return pixsim.Refund{}, pixsim.ErrKeyReused
		}
		return rec.refund, nil
	}
	chargeKey, ok := s.keyByID[chargeID]
	if !ok {
		return pixsim.Refund{}, pixsim.ErrNotFound
	}
	charge := s.byKey[chargeKey]

	rf := pixsim.Refund{
		ID:             "rfd_" + simkit.Text(s.seed.RNG("refund\x00"+key), idLength),
		IdempotencyKey: key,
		ChargeID:       chargeID,
		Status:         pixsim.RefundRejected,
		Amount:         req.Amount,
		CreatedAt:      s.clock.Now(),
	}
	switch {
	case reject:
		rf.Reason = "the refund was refused"
	case charge.charge.Status != pixsim.Paid:
		rf.Reason = "only a paid charge can be refunded"
	case req.Amount.Currency() != charge.charge.Amount.Currency():
		rf.Reason = fmt.Sprintf("the charge is in %s", charge.charge.Amount.Currency())
	case req.Amount.Amount() > charge.charge.Amount.Amount()-charge.refunded:
		rf.Reason = fmt.Sprintf("only %d of the charge is left to refund", charge.charge.Amount.Amount()-charge.refunded)
	default:
		rf.Status = pixsim.RefundCompleted
		charge.refunded += req.Amount.Amount()
	}
	s.refunds[key] = &refundRecord{req: *req, refund: rf}
	return rf, nil
}

// pay plays the payer. A static PIX code can be paid at any time, so a charge past
// its expiry is paid too: that is the late payment of decision 8. Only the payment
// itself sends charge.paid; paying a paid charge again is no new fact.
func (s *Simulator) pay(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	key, ok := s.keyByID[r.PathValue("id")]
	var ch pixsim.Charge
	var event []byte
	if ok {
		rec := s.byKey[key]
		if rec.charge.Status == pixsim.Active {
			paidAt := s.clock.Now()
			rec.charge.Status = pixsim.Paid
			rec.charge.PaidAt = &paidAt
			e := pixsim.Event{
				ID: "evt_" + simkit.Text(s.seed.RNG(string(pixsim.ChargePaid)+"\x00"+key), idLength), Type: pixsim.ChargePaid,
				CreatedAt: paidAt, Charge: rec.charge,
			}
			event = s.webhook.Record(e.ID, &e)
		}
		ch = rec.charge
	}
	s.mu.Unlock()

	if event != nil {
		// The payer is done once paid; a client that gives up must not cut the delivery.
		if err := s.webhook.Deliver(context.WithoutCancel(r.Context()), event); err != nil {
			s.logger.Warn("event delivery failed", "charge", ch.ID, "error", err)
		}
	}
	switch {
	case !ok:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, pixsim.CodeNotFound, "no charge with this ID"))
	case ch.Status == pixsim.Rejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, pixsim.CodeNotPayable, "a rejected charge cannot be paid"))
	default:
		simkit.WriteJSON(w, http.StatusOK, ch)
	}
}

// store creates the charge once per key. Every random choice comes from a generator
// seeded by the simulator's seed and the key, so a seed gives the same charges
// whatever order concurrent requests arrive in.
func (s *Simulator) store(key string, req *pixsim.CreateCharge, reject bool) (pixsim.Charge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.byKey[key]; ok {
		if rec.req.Amount != req.Amount || !rec.req.ExpiresAt.Equal(req.ExpiresAt) {
			return pixsim.Charge{}, pixsim.ErrKeyReused
		}
		return rec.charge, nil
	}

	rng := s.seed.RNG(key)
	id := "chg_" + simkit.Text(rng, idLength)
	for s.keyByID[id] != "" {
		id = "chg_" + simkit.Text(rng, idLength)
	}
	ch := pixsim.Charge{
		ID:             id,
		IdempotencyKey: key,
		Status:         pixsim.Rejected,
		Amount:         req.Amount,
		ExpiresAt:      req.ExpiresAt.UTC(),
		CreatedAt:      s.clock.Now(),
	}
	if !reject {
		ch.Status = pixsim.Active
		ch.TxID = simkit.Text(rng, brcode.MaxTxID)
		code := brcode.Code{
			Key: s.cfg.PixKey, Amount: req.Amount, TxID: ch.TxID,
			MerchantName: s.cfg.MerchantName, MerchantCity: s.cfg.MerchantCity,
		}
		payload, err := brcode.Encode(&code)
		if err != nil {
			return pixsim.Charge{}, fmt.Errorf("br code: %w", err)
		}
		ch.BRCode = payload
	}
	s.byKey[key] = &record{req: *req, charge: ch}
	s.keyByID[id] = key
	return ch, nil
}

func validateCharge(req *pixsim.CreateCharge) error {
	switch {
	case req.Amount.Currency() != money.BRL:
		return fmt.Errorf("amount must be in BRL, got %s", req.Amount.Currency())
	case req.Amount.Amount() <= 0:
		return fmt.Errorf("amount must be positive, got %s", req.Amount)
	case req.ExpiresAt.IsZero():
		return errors.New("expires_at is required")
	}
	return nil
}
