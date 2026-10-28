// Package server is the PIX simulator: a provider that keeps its charges in memory,
// derives every random choice from a seed and fails on command.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/brcode"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/providers/pixsim"
)

// HangLimit bounds a TimeoutAfter fault, in case the caller never gives up.
const HangLimit = time.Minute

// DeliveryTimeout bounds one webhook delivery.
const DeliveryTimeout = 5 * time.Second

const (
	maxKeyLength   = 255
	maxFaultCount  = 1000
	maxDelay       = time.Minute
	idLength       = 20
	maxRequestBody = 1 << 16
)

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

type Clock interface {
	Now() time.Time
}

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
	cfg    Config
	clock  Clock
	logger *slog.Logger
	http   *http.Client

	mu      sync.Mutex
	byKey   map[string]*record
	keyByID map[string]string
	faults  []pixsim.Fault
	refunds map[string]*refundRecord
	// events holds each event already encoded, so a redelivery sends the same bytes.
	events map[string][]byte
}

func New(cfg *Config, clock Clock, logger *slog.Logger) (*Simulator, error) {
	probe := brcode.Code{Key: cfg.PixKey, MerchantName: cfg.MerchantName, MerchantCity: cfg.MerchantCity}
	if _, err := brcode.Encode(&probe); err != nil {
		return nil, fmt.Errorf("pixsim: receiver: %w", err)
	}
	if cfg.WebhookURL != "" && len(cfg.WebhookSecret) == 0 {
		return nil, errors.New("pixsim: a webhook URL needs a webhook secret")
	}
	return &Simulator{
		cfg: *cfg, clock: clock, logger: logger, http: &http.Client{Timeout: DeliveryTimeout},
		byKey: map[string]*record{}, keyByID: map[string]string{}, events: map[string][]byte{},
		refunds: map[string]*refundRecord{},
	}, nil
}

func (s *Simulator) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/charges", s.create)
	mux.HandleFunc("GET /v1/charges/by-key/{key}", s.get)
	mux.HandleFunc("POST /v1/charges/{id}/refunds", s.refund)
	mux.HandleFunc("GET /v1/refunds/by-key/{key}", s.getRefund)
	mux.HandleFunc("POST /control/charges/{id}/pay", s.pay)
	mux.HandleFunc("POST /control/faults", s.injectFault)
	mux.HandleFunc("POST /control/events/{id}/deliver", s.redeliver)
}

func (s *Simulator) create(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if !validKey(key) {
		invalid(w, r, "Idempotency-Key must have 1 to 255 printable ASCII characters")
		return
	}
	var req pixsim.CreateCharge
	if err := decode(w, r, &req); err != nil {
		invalid(w, r, err.Error())
		return
	}
	if err := validateCharge(&req); err != nil {
		invalid(w, r, err.Error())
		return
	}

	fault, _ := s.nextFault(pixsim.OpCreate)
	if !s.before(w, r, fault) {
		return
	}
	ch, err := s.store(key, &req, fault.Kind == pixsim.Reject)
	if fault.Kind == pixsim.TimeoutAfter {
		hang(r)
	}
	switch {
	case errors.Is(err, pixsim.ErrKeyReused):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, pixsim.CodeKeyReused, err.Error()))
	case err != nil:
		invalid(w, r, err.Error())
	case ch.Status == pixsim.Rejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, pixsim.CodeRejected, "the charge was refused"))
	default:
		writeJSON(w, http.StatusCreated, ch)
	}
}

func (s *Simulator) get(w http.ResponseWriter, r *http.Request) {
	fault, _ := s.nextFault(pixsim.OpGet)
	if !s.before(w, r, fault) {
		return
	}
	if fault.Kind == pixsim.TimeoutAfter {
		hang(r)
	}
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
	writeJSON(w, http.StatusOK, ch)
}

func (s *Simulator) refund(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if !validKey(key) {
		invalid(w, r, "Idempotency-Key must have 1 to 255 printable ASCII characters")
		return
	}
	var req pixsim.CreateRefund
	if err := decode(w, r, &req); err != nil {
		invalid(w, r, err.Error())
		return
	}
	if req.Amount.Amount() <= 0 {
		invalid(w, r, fmt.Sprintf("amount must be positive, got %s", req.Amount))
		return
	}

	fault, _ := s.nextFault(pixsim.OpRefund)
	if !s.before(w, r, fault) {
		return
	}
	rf, err := s.storeRefund(key, r.PathValue("id"), &req, fault.Kind == pixsim.Reject)
	if fault.Kind == pixsim.TimeoutAfter {
		hang(r)
	}
	switch {
	case errors.Is(err, pixsim.ErrKeyReused):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, pixsim.CodeKeyReused, err.Error()))
	case errors.Is(err, pixsim.ErrNotFound):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, pixsim.CodeNotFound, "no charge with this ID"))
	case err != nil:
		invalid(w, r, err.Error())
	case rf.Status == pixsim.RefundRejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, pixsim.CodeRefundRejected, rf.Reason))
	default:
		writeJSON(w, http.StatusCreated, rf)
	}
}

func (s *Simulator) getRefund(w http.ResponseWriter, r *http.Request) {
	fault, _ := s.nextFault(pixsim.OpGetRefund)
	if !s.before(w, r, fault) {
		return
	}
	if fault.Kind == pixsim.TimeoutAfter {
		hang(r)
	}
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
	writeJSON(w, http.StatusOK, rf)
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
		ID:             "rfd_" + text(s.rng("refund\x00"+key), idLength),
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
			event = s.record(&pixsim.Event{
				ID: "evt_" + text(s.rng(string(pixsim.ChargePaid)+"\x00"+key), idLength), Type: pixsim.ChargePaid,
				CreatedAt: paidAt, Charge: rec.charge,
			})
		}
		ch = rec.charge
	}
	s.mu.Unlock()

	if event != nil {
		// The payer is done once paid; a client that gives up must not cut the delivery.
		if err := s.deliver(context.WithoutCancel(r.Context()), event); err != nil {
			s.logger.Warn("event delivery failed", "charge", ch.ID, "error", err)
		}
	}
	switch {
	case !ok:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, pixsim.CodeNotFound, "no charge with this ID"))
	case ch.Status == pixsim.Rejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, pixsim.CodeNotPayable, "a rejected charge cannot be paid"))
	default:
		writeJSON(w, http.StatusOK, ch)
	}
}

func (s *Simulator) redeliver(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	event, ok := s.events[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, pixsim.CodeEventNotFound, "no event with this ID"))
		return
	}
	if err := s.deliver(r.Context(), event); err != nil {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusBadGateway, pixsim.CodeDeliveryFailed, err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// record encodes and keeps e; the caller holds s.mu. It returns nil when no webhook
// is configured, so nothing is sent.
func (s *Simulator) record(e *pixsim.Event) []byte {
	if s.cfg.WebhookURL == "" {
		return nil
	}
	body, err := json.Marshal(e)
	if err != nil {
		panic(fmt.Sprintf("pixsim: encode event: %v", err)) // only plain fields: cannot fail
	}
	s.events[e.ID] = body
	return body
}

// deliver posts body to the webhook, signed at the current time. Any 2xx counts.
func (s *Simulator) deliver(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(pixsim.SignatureHeader, pixsim.Sign(s.cfg.WebhookSecret, s.clock.Now(), body))
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRequestBody))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook answered %d", resp.StatusCode)
	}
	return nil
}

func (s *Simulator) injectFault(w http.ResponseWriter, r *http.Request) {
	var f pixsim.Fault
	if err := decode(w, r, &f); err != nil {
		invalid(w, r, err.Error())
		return
	}
	if err := validateFault(&f); err != nil {
		invalid(w, r, err.Error())
		return
	}
	s.mu.Lock()
	s.faults = append(s.faults, f)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// nextFault takes one use of the oldest fault scheduled for op.
func (s *Simulator) nextFault(op pixsim.Op) (pixsim.Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.faults {
		if s.faults[i].Op != op {
			continue
		}
		f := s.faults[i]
		s.faults[i].Count--
		if s.faults[i].Count == 0 {
			s.faults = append(s.faults[:i], s.faults[i+1:]...)
		}
		return f, true
	}
	return pixsim.Fault{}, false
}

// before runs the part of a fault that happens before any work. It reports whether
// the handler should go on.
func (s *Simulator) before(w http.ResponseWriter, r *http.Request, f pixsim.Fault) bool {
	switch f.Kind {
	case pixsim.FailBefore:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusInternalServerError, pixsim.CodeFailedFault, "injected failure"))
		return false
	case pixsim.Delay:
		t := time.NewTimer(time.Duration(f.DelayMS) * time.Millisecond)
		defer t.Stop()
		select {
		case <-r.Context().Done():
			return false
		case <-t.C:
			return true
		}
	case pixsim.Reject, pixsim.TimeoutAfter:
		return true
	default:
		return true
	}
}

// hang never answers: it waits for the caller to give up, or HangLimit, and then
// drops the connection. net/http treats ErrAbortHandler as a deliberate abort.
func hang(r *http.Request) {
	t := time.NewTimer(HangLimit)
	defer t.Stop()
	select {
	case <-r.Context().Done():
	case <-t.C:
	}
	panic(http.ErrAbortHandler)
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

	rng := s.rng(key)
	id := "chg_" + text(rng, idLength)
	for s.keyByID[id] != "" {
		id = "chg_" + text(rng, idLength)
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
		ch.TxID = text(rng, brcode.MaxTxID)
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

func (s *Simulator) rng(key string) *rand.Rand {
	sum := sha256.Sum256([]byte(key))
	return rand.New(rand.NewPCG(s.cfg.Seed, binary.BigEndian.Uint64(sum[:8]))) //nolint:gosec // G404: determinism from the seed is the point; nothing here is a secret.
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func text(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}

func validKey(key string) bool {
	if key == "" || len(key) > maxKeyLength {
		return false
	}
	for i := range len(key) {
		if key[i] < 0x20 || key[i] > 0x7e {
			return false
		}
	}
	return true
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

func validateFault(f *pixsim.Fault) error {
	switch f.Op {
	case pixsim.OpCreate, pixsim.OpGet, pixsim.OpRefund, pixsim.OpGetRefund:
	default:
		return fmt.Errorf("op must be create, get, refund or get_refund, got %q", f.Op)
	}
	switch f.Kind {
	case pixsim.Reject:
		if f.Op != pixsim.OpCreate && f.Op != pixsim.OpRefund {
			return errors.New("only a create or a refund can be rejected")
		}
	case pixsim.Delay:
		if d := time.Duration(f.DelayMS) * time.Millisecond; d <= 0 || d > maxDelay {
			return fmt.Errorf("delay_ms must be between 1 and %d", maxDelay.Milliseconds())
		}
	case pixsim.FailBefore, pixsim.TimeoutAfter:
	default:
		return fmt.Errorf("unknown fault %q", f.Kind)
	}
	if f.Kind != pixsim.Delay && f.DelayMS != 0 {
		return errors.New("delay_ms only applies to the delay fault")
	}
	if f.Count < 1 || f.Count > maxFaultCount {
		return fmt.Errorf("count must be between 1 and %d", maxFaultCount)
	}
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func invalid(w http.ResponseWriter, r *http.Request, detail string) {
	httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusBadRequest, pixsim.CodeInvalid, detail))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
