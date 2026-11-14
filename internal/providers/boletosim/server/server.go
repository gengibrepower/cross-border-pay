// Package server is the boleto simulator: a bank that keeps its boletos in memory,
// derives every random choice from a seed and fails on command. It receives a boleto
// until the end of the first banking day on or after its due date and refuses it
// after (decision 53), by the calendar of decision 21.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gengibrepower/cross-border-pay/internal/boleto"
	"github.com/gengibrepower/cross-border-pay/internal/calendar"
	"github.com/gengibrepower/cross-border-pay/internal/cpf"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/providers/boletosim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

const (
	idLength = 20
	// The simulator's free field: the beneficiary's code, the gateway's account at the
	// bank, then the nosso número, the bank's number for the boleto.
	BeneficiaryLength = 8
	nossoNumeroLength = boleto.FreeFieldLen - BeneficiaryLength
	maxPayerName      = 100
	maxEmail          = 254
)

type Config struct {
	Seed uint64
	// Bank is the COMPE code printed on every boleto, and Beneficiary the gateway's
	// account at it.
	Bank        string
	Beneficiary string
	// WebhookURL receives an event, signed with WebhookSecret, for every boleto paid.
	// Empty means no events are sent.
	WebhookURL    string
	WebhookSecret []byte
}

type Clock = simkit.Clock

type record struct {
	req    boletosim.CreateBoleto
	boleto boletosim.Boleto
	// refunded is what completed refunds returned, in minor units.
	refunded int64
}

type refundRecord struct {
	req    boletosim.CreateRefund
	refund boletosim.Refund
}

type Simulator struct {
	cfg      Config
	seed     simkit.Seed
	clock    Clock
	calendar calendar.Calendar
	logger   *slog.Logger
	faults   *simkit.Faults
	webhook  *simkit.Webhook

	mu      sync.Mutex
	byKey   map[string]*record
	keyByID map[string]string
	refunds map[string]*refundRecord
}

func New(cfg *Config, clock Clock, cal calendar.Calendar, logger *slog.Logger) (*Simulator, error) {
	probe := boleto.Boleto{Bank: cfg.Bank, FreeField: cfg.Beneficiary + strings.Repeat("0", nossoNumeroLength)}
	if len(cfg.Beneficiary) != BeneficiaryLength {
		return nil, fmt.Errorf("boletosim: beneficiary must be %d digits, got %q", BeneficiaryLength, cfg.Beneficiary)
	}
	probe.Amount, probe.Due = money.New(1, money.BRL), cal.Today(clock.Now())
	if _, err := boleto.Barcode(&probe); err != nil {
		return nil, fmt.Errorf("boletosim: %w", err)
	}
	webhook, err := simkit.NewWebhook(cfg.WebhookURL, cfg.WebhookSecret, boletosim.SignatureHeader, clock)
	if err != nil {
		return nil, fmt.Errorf("boletosim: %w", err)
	}
	return &Simulator{
		cfg: *cfg, seed: simkit.Seed(cfg.Seed), clock: clock, calendar: cal, logger: logger,
		faults: simkit.NewFaults(boletosim.Ops, boletosim.Rejectable), webhook: webhook,
		byKey: map[string]*record{}, keyByID: map[string]string{}, refunds: map[string]*refundRecord{},
	}, nil
}

func (s *Simulator) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/boletos", s.create)
	mux.HandleFunc("GET /v1/boletos/by-key/{key}", s.get)
	mux.HandleFunc("POST /v1/boletos/{id}/refunds", s.refund)
	mux.HandleFunc("GET /v1/refunds/by-key/{key}", s.getRefund)
	mux.HandleFunc("POST /control/boletos/{id}/pay", s.pay)
	mux.HandleFunc("POST /control/faults", s.faults.InjectFault)
	mux.HandleFunc("POST /control/events/{id}/deliver", s.webhook.Redeliver)
}

func (s *Simulator) create(w http.ResponseWriter, r *http.Request) {
	key, ok := simkit.IdempotencyKey(w, r)
	if !ok {
		return
	}
	var req boletosim.CreateBoleto
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if err := s.validate(&req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}

	fault := s.faults.Next(boletosim.OpCreate)
	if !simkit.Before(w, r, &fault) {
		return
	}
	b, err := s.store(key, &req, fault.Kind == boletosim.Reject)
	simkit.After(r, &fault)
	switch {
	case errors.Is(err, boletosim.ErrKeyReused):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, boletosim.CodeKeyReused, err.Error()))
	case err != nil:
		simkit.Invalid(w, r, err.Error())
	case b.Status == boletosim.Rejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, boletosim.CodeRejected, "the bank refused to register the boleto"))
	default:
		simkit.WriteJSON(w, http.StatusCreated, b)
	}
}

func (s *Simulator) validate(req *boletosim.CreateBoleto) error {
	name := strings.TrimSpace(req.Payer.Name)
	switch {
	case req.Amount.Currency() != money.BRL:
		return fmt.Errorf("amount must be in BRL, got %s", req.Amount.Currency())
	case req.Amount.Amount() <= 0 || req.Amount.Amount() > boleto.MaxAmount:
		return fmt.Errorf("amount must be from 0.01 to 99999999.99, got %s", req.Amount)
	case req.DueDate.IsZero():
		return errors.New("due_date is required")
	case req.DueDate.Before(s.calendar.Today(s.clock.Now())):
		return fmt.Errorf("due_date %s is in the past", req.DueDate)
	case name == "" || utf8.RuneCountInString(name) > maxPayerName:
		return fmt.Errorf("payer.name must have 1 to %d characters", maxPayerName)
	case len(req.Payer.Email) > maxEmail:
		return fmt.Errorf("payer.email must have at most %d characters", maxEmail)
	}
	if _, err := cpf.Parse(req.Payer.Document); err != nil {
		return fmt.Errorf("payer.document: %w", err)
	}
	if req.Payer.Email != "" {
		if _, err := mail.ParseAddress(req.Payer.Email); err != nil {
			return fmt.Errorf("payer.email: %w", err)
		}
	}
	return nil
}

// store registers the boleto once per key. Every random choice comes from a generator
// seeded by the simulator's seed and the key.
func (s *Simulator) store(key string, req *boletosim.CreateBoleto, reject bool) (boletosim.Boleto, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.byKey[key]; ok {
		if rec.req != *req {
			return boletosim.Boleto{}, boletosim.ErrKeyReused
		}
		return s.current(rec), nil
	}

	rng := s.seed.RNG(key)
	id := "bol_" + simkit.Text(rng, idLength)
	for s.keyByID[id] != "" {
		id = "bol_" + simkit.Text(rng, idLength)
	}
	b := boletosim.Boleto{
		ID: id, IdempotencyKey: key, Status: boletosim.Rejected,
		Amount: req.Amount, DueDate: req.DueDate, CreatedAt: s.clock.Now(),
	}
	if !reject {
		b.Status = boletosim.Registered
		b.NossoNumero = digits(rng, nossoNumeroLength)
		code, err := boleto.Barcode(&boleto.Boleto{
			Bank: s.cfg.Bank, Due: req.DueDate, Amount: req.Amount, FreeField: s.cfg.Beneficiary + b.NossoNumero,
		})
		if err != nil {
			return boletosim.Boleto{}, fmt.Errorf("barcode: %w", err)
		}
		line, err := boleto.Line(code)
		if err != nil {
			return boletosim.Boleto{}, fmt.Errorf("digitable line: %w", err)
		}
		b.Barcode, b.DigitableLine = code, line
	}
	s.byKey[key] = &record{req: *req, boleto: b}
	s.keyByID[id] = key
	return b, nil
}

// current is the boleto as of now; the caller holds s.mu. A boleto never paid whose
// last day went by has expired, whoever asks first: every read checks the date.
func (s *Simulator) current(rec *record) boletosim.Boleto {
	if rec.boleto.Status == boletosim.Registered && !s.payable(rec.boleto.DueDate) {
		rec.boleto.Status = boletosim.Expired
	}
	return rec.boleto
}

// payable is whether today, at the bank, is no later than the first banking day on or
// after the due date: a boleto due on a Saturday is still received on Monday.
func (s *Simulator) payable(due calendar.Date) bool {
	return !s.calendar.Today(s.clock.Now()).After(s.calendar.OnOrAfter(due))
}

func (s *Simulator) get(w http.ResponseWriter, r *http.Request) {
	fault := s.faults.Next(boletosim.OpGet)
	if !simkit.Before(w, r, &fault) {
		return
	}
	simkit.After(r, &fault)
	s.mu.Lock()
	rec, ok := s.byKey[r.PathValue("key")]
	var b boletosim.Boleto
	if ok {
		b = s.current(rec)
	}
	s.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, boletosim.CodeNotFound, "no boleto with this idempotency key"))
		return
	}
	simkit.WriteJSON(w, http.StatusOK, b)
}

// pay plays the payer. Only the payment itself sends boleto.paid; paying a paid boleto
// again is no new fact.
func (s *Simulator) pay(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	key, ok := s.keyByID[r.PathValue("id")]
	var b boletosim.Boleto
	var event []byte
	if ok {
		rec := s.byKey[key]
		if s.current(rec).Status == boletosim.Registered {
			paidAt := s.clock.Now()
			rec.boleto.Status = boletosim.Paid
			rec.boleto.PaidAt = &paidAt
			e := boletosim.Event{
				ID:   "evt_" + simkit.Text(s.seed.RNG(string(boletosim.BoletoPaid)+"\x00"+key), idLength),
				Type: boletosim.BoletoPaid, CreatedAt: paidAt, Boleto: rec.boleto,
			}
			event = s.webhook.Record(e.ID, &e)
		}
		b = rec.boleto
	}
	s.mu.Unlock()

	if event != nil {
		// The payer is done once paid; a client that gives up must not cut the delivery.
		if err := s.webhook.Deliver(context.WithoutCancel(r.Context()), event); err != nil {
			s.logger.Warn("event delivery failed", "boleto", b.ID, "error", err)
		}
	}
	switch {
	case !ok:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, boletosim.CodeNotFound, "no boleto with this ID"))
	case b.Status == boletosim.Rejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, boletosim.CodeNotPayable, "a rejected boleto cannot be paid"))
	case b.Status == boletosim.Expired:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, boletosim.CodeOverdue,
			fmt.Sprintf("the boleto was due on %s and is not received after it", b.DueDate)))
	default:
		simkit.WriteJSON(w, http.StatusOK, b)
	}
}

func (s *Simulator) refund(w http.ResponseWriter, r *http.Request) {
	key, ok := simkit.IdempotencyKey(w, r)
	if !ok {
		return
	}
	var req boletosim.CreateRefund
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if req.Amount.Amount() <= 0 {
		simkit.Invalid(w, r, fmt.Sprintf("amount must be positive, got %s", req.Amount))
		return
	}

	fault := s.faults.Next(boletosim.OpRefund)
	if !simkit.Before(w, r, &fault) {
		return
	}
	rf, err := s.storeRefund(key, r.PathValue("id"), &req, fault.Kind == boletosim.Reject)
	simkit.After(r, &fault)
	switch {
	case errors.Is(err, boletosim.ErrKeyReused):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, boletosim.CodeKeyReused, err.Error()))
	case errors.Is(err, boletosim.ErrNotFound):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, boletosim.CodeNotFound, "no boleto with this ID"))
	case err != nil:
		simkit.Invalid(w, r, err.Error())
	case rf.Status == boletosim.RefundRejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, boletosim.CodeRefundRejected, rf.Reason))
	default:
		simkit.WriteJSON(w, http.StatusCreated, rf)
	}
}

func (s *Simulator) getRefund(w http.ResponseWriter, r *http.Request) {
	fault := s.faults.Next(boletosim.OpGetRefund)
	if !simkit.Before(w, r, &fault) {
		return
	}
	simkit.After(r, &fault)
	s.mu.Lock()
	rec, ok := s.refunds[r.PathValue("key")]
	var rf boletosim.Refund
	if ok {
		rf = rec.refund
	}
	s.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, boletosim.CodeRefundNotFound, "no refund with this idempotency key"))
		return
	}
	simkit.WriteJSON(w, http.StatusOK, rf)
}

// storeRefund decides the refund once per key and keeps it, rejected too. The bank
// never returns more than the boleto took in (invariant 8).
func (s *Simulator) storeRefund(key, boletoID string, req *boletosim.CreateRefund, reject bool) (boletosim.Refund, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.refunds[key]; ok {
		if rec.refund.BoletoID != boletoID || rec.req.Amount != req.Amount {
			return boletosim.Refund{}, boletosim.ErrKeyReused
		}
		return rec.refund, nil
	}
	boletoKey, ok := s.keyByID[boletoID]
	if !ok {
		return boletosim.Refund{}, boletosim.ErrNotFound
	}
	paid := s.byKey[boletoKey]

	rf := boletosim.Refund{
		ID:             "rfd_" + simkit.Text(s.seed.RNG("refund\x00"+key), idLength),
		IdempotencyKey: key, BoletoID: boletoID, Status: boletosim.RefundRejected,
		Amount: req.Amount, CreatedAt: s.clock.Now(),
	}
	switch {
	case reject:
		rf.Reason = "the refund was refused"
	case paid.boleto.Status != boletosim.Paid:
		rf.Reason = "only a paid boleto can be refunded"
	case req.Amount.Currency() != paid.boleto.Amount.Currency():
		rf.Reason = fmt.Sprintf("the boleto is in %s", paid.boleto.Amount.Currency())
	case req.Amount.Amount() > paid.boleto.Amount.Amount()-paid.refunded:
		rf.Reason = fmt.Sprintf("only %d of the boleto is left to refund", paid.boleto.Amount.Amount()-paid.refunded)
	default:
		rf.Status = boletosim.RefundCompleted
		paid.refunded += req.Amount.Amount()
	}
	s.refunds[key] = &refundRecord{req: *req, refund: rf}
	return rf, nil
}

func digits(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = "0123456789"[rng.IntN(10)]
	}
	return string(b)
}
