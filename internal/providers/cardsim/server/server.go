// Package server is the card simulator: an acquirer that keeps its authorizations in
// memory, derives every random choice from a seed and fails on command.
package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

const idLength = 20

type Config struct {
	Seed uint64
}

type Clock = simkit.Clock

type authRecord struct {
	req  cardsim.Authorize
	auth cardsim.Authorization
	// refunded is what completed refunds returned, in minor units.
	refunded int64
}

type refundRecord struct {
	req    cardsim.CreateRefund
	refund cardsim.Refund
}

type Simulator struct {
	seed   simkit.Seed
	clock  Clock
	logger *slog.Logger
	faults *simkit.Faults

	mu      sync.Mutex
	byKey   map[string]*authRecord
	keyByID map[string]string
	refunds map[string]*refundRecord
}

func New(cfg *Config, clock Clock, logger *slog.Logger) *Simulator {
	return &Simulator{
		seed: simkit.Seed(cfg.Seed), clock: clock, logger: logger,
		faults: simkit.NewFaults(cardsim.Ops, cardsim.Rejectable),
		byKey:  map[string]*authRecord{}, keyByID: map[string]string{}, refunds: map[string]*refundRecord{},
	}
}

func (s *Simulator) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/authorizations", s.authorize)
	mux.HandleFunc("GET /v1/authorizations/by-key/{key}", s.get)
	mux.HandleFunc("POST /v1/authorizations/{id}/capture", s.capture)
	mux.HandleFunc("POST /v1/authorizations/{id}/void", s.void)
	mux.HandleFunc("POST /v1/authorizations/{id}/refunds", s.refund)
	mux.HandleFunc("GET /v1/refunds/by-key/{key}", s.getRefund)
	mux.HandleFunc("POST /control/faults", s.faults.InjectFault)
}

func (s *Simulator) authorize(w http.ResponseWriter, r *http.Request) {
	key, ok := simkit.IdempotencyKey(w, r)
	if !ok {
		return
	}
	var req cardsim.Authorize
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if err := validateAuthorize(&req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}

	fault := s.faults.Next(cardsim.OpAuthorize)
	if !simkit.Before(w, r, &fault) {
		return
	}
	a, err := s.store(key, &req, fault.Kind == cardsim.Reject)
	simkit.After(r, &fault)
	if errors.Is(err, cardsim.ErrKeyReused) {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, cardsim.CodeKeyReused, err.Error()))
		return
	}
	simkit.WriteJSON(w, http.StatusCreated, a)
}

func (s *Simulator) get(w http.ResponseWriter, r *http.Request) {
	fault := s.faults.Next(cardsim.OpGet)
	if !simkit.Before(w, r, &fault) {
		return
	}
	simkit.After(r, &fault)
	s.mu.Lock()
	rec, ok := s.byKey[r.PathValue("key")]
	var a cardsim.Authorization
	if ok {
		a = s.current(rec)
	}
	s.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeNotFound, "no authorization with this idempotency key"))
		return
	}
	simkit.WriteJSON(w, http.StatusOK, a)
}

// store decides the authorization once per key. Every random choice comes from a
// generator seeded by the simulator's seed and the key.
func (s *Simulator) store(key string, req *cardsim.Authorize, reject bool) (cardsim.Authorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.byKey[key]; ok {
		if rec.req != *req {
			return cardsim.Authorization{}, cardsim.ErrKeyReused
		}
		return s.current(rec), nil
	}

	rng := s.seed.RNG(key)
	id := "auth_" + simkit.Text(rng, idLength)
	for s.keyByID[id] != "" {
		id = "auth_" + simkit.Text(rng, idLength)
	}
	now := s.clock.Now()
	a := cardsim.Authorization{
		ID: id, IdempotencyKey: key, Status: cardsim.Authorized, Amount: req.Amount,
		Captured: money.New(0, req.Amount.Currency()), CreatedAt: now, ExpiresAt: now.Add(cardsim.AuthorizationValidity),
	}
	switch {
	case reject:
		a.Status, a.DeclineReason = cardsim.Declined, "do_not_honor"
	case req.Token == cardsim.TokenDeclined:
		a.Status, a.DeclineReason = cardsim.Declined, "card_declined"
	case req.Token == cardsim.TokenInsufficientFunds:
		a.Status, a.DeclineReason = cardsim.Declined, "insufficient_funds"
	case req.Token != cardsim.TokenApproved:
		a.Status, a.DeclineReason = cardsim.Declined, "invalid_token"
	case req.Capture:
		a.Status, a.Captured, a.CapturedAt = cardsim.Captured, req.Amount, &now
	}
	s.byKey[key] = &authRecord{req: *req, auth: a}
	s.keyByID[id] = key
	return a, nil
}

// current is the authorization as of now; the caller holds s.mu. An authorization
// past its validity has expired, whoever asks first: every read checks the deadline.
func (s *Simulator) current(rec *authRecord) cardsim.Authorization {
	if rec.auth.Status == cardsim.Authorized && !s.clock.Now().Before(rec.auth.ExpiresAt) {
		rec.auth.Status = cardsim.Expired
	}
	return rec.auth
}

// capture charges once: the same amount again returns the same authorization, as a
// retry after a lost answer needs; another amount, more than was authorized, or an
// authorization that is not live is refused.
func (s *Simulator) capture(w http.ResponseWriter, r *http.Request) {
	var req cardsim.Capture
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if req.Amount.Amount() <= 0 {
		simkit.Invalid(w, r, fmt.Sprintf("amount must be positive, got %s", req.Amount))
		return
	}
	fault := s.faults.Next(cardsim.OpCapture)
	if !simkit.Before(w, r, &fault) {
		return
	}
	a, found, refusal := s.doCapture(r.PathValue("id"), req.Amount)
	simkit.After(r, &fault)
	switch {
	case !found:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeNotFound, "no authorization with this ID"))
	case refusal != "":
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, cardsim.CodeNotCapturable, refusal))
	default:
		simkit.WriteJSON(w, http.StatusOK, a)
	}
}

func (s *Simulator) doCapture(id string, amount money.Money) (a cardsim.Authorization, found bool, refusal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keyByID[id]
	if !ok {
		return cardsim.Authorization{}, false, ""
	}
	rec := s.byKey[key]
	a = s.current(rec)
	switch {
	case a.Status == cardsim.Captured && a.Captured == amount:
		return a, true, ""
	case a.Status != cardsim.Authorized:
		return a, true, fmt.Sprintf("the authorization is %s", a.Status)
	case amount.Currency() != a.Amount.Currency():
		return a, true, fmt.Sprintf("the authorization is in %s", a.Amount.Currency())
	case amount.Amount() > a.Amount.Amount():
		return a, true, fmt.Sprintf("only %d was authorized", a.Amount.Amount())
	}
	now := s.clock.Now()
	rec.auth.Status, rec.auth.Captured, rec.auth.CapturedAt = cardsim.Captured, amount, &now
	return rec.auth, true, ""
}

// void releases a live authorization; a voided one is returned as it is, so a retry
// after a lost answer gets the same answer, and any other status is refused.
func (s *Simulator) void(w http.ResponseWriter, r *http.Request) {
	fault := s.faults.Next(cardsim.OpVoid)
	if !simkit.Before(w, r, &fault) {
		return
	}
	a, found, refusal := s.doVoid(r.PathValue("id"))
	simkit.After(r, &fault)
	switch {
	case !found:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeNotFound, "no authorization with this ID"))
	case refusal != "":
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, cardsim.CodeNotVoidable, refusal))
	default:
		simkit.WriteJSON(w, http.StatusOK, a)
	}
}

func (s *Simulator) doVoid(id string) (a cardsim.Authorization, found bool, refusal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keyByID[id]
	if !ok {
		return cardsim.Authorization{}, false, ""
	}
	rec := s.byKey[key]
	a = s.current(rec)
	switch a.Status {
	case cardsim.Voided:
		return a, true, ""
	case cardsim.Authorized:
		rec.auth.Status = cardsim.Voided
		return rec.auth, true, ""
	case cardsim.Captured, cardsim.Declined, cardsim.Expired:
		return a, true, fmt.Sprintf("the authorization is %s", a.Status)
	default:
		return a, true, fmt.Sprintf("the authorization is %s", a.Status)
	}
}

func (s *Simulator) refund(w http.ResponseWriter, r *http.Request) {
	key, ok := simkit.IdempotencyKey(w, r)
	if !ok {
		return
	}
	var req cardsim.CreateRefund
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if req.Amount.Amount() <= 0 {
		simkit.Invalid(w, r, fmt.Sprintf("amount must be positive, got %s", req.Amount))
		return
	}
	fault := s.faults.Next(cardsim.OpRefund)
	if !simkit.Before(w, r, &fault) {
		return
	}
	rf, err := s.storeRefund(key, r.PathValue("id"), &req, fault.Kind == cardsim.Reject)
	simkit.After(r, &fault)
	switch {
	case errors.Is(err, cardsim.ErrKeyReused):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, cardsim.CodeKeyReused, err.Error()))
	case errors.Is(err, cardsim.ErrNotFound):
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeNotFound, "no authorization with this ID"))
	case rf.Status == cardsim.RefundRejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusUnprocessableEntity, cardsim.CodeRefundRejected, rf.Reason))
	default:
		simkit.WriteJSON(w, http.StatusCreated, rf)
	}
}

// storeRefund decides the refund once per key and keeps it, rejected too. Like the
// PIX simulator, it holds invariant 8 on its own side: never more than was captured.
func (s *Simulator) storeRefund(key, id string, req *cardsim.CreateRefund, reject bool) (cardsim.Refund, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.refunds[key]; ok {
		if rec.refund.AuthorizationID != id || rec.req.Amount != req.Amount {
			return cardsim.Refund{}, cardsim.ErrKeyReused
		}
		return rec.refund, nil
	}
	authKey, ok := s.keyByID[id]
	if !ok {
		return cardsim.Refund{}, cardsim.ErrNotFound
	}
	auth := s.byKey[authKey]
	a := s.current(auth)

	rf := cardsim.Refund{
		ID:             "rfd_" + simkit.Text(s.seed.RNG("refund\x00"+key), idLength),
		IdempotencyKey: key, AuthorizationID: id, Status: cardsim.RefundRejected,
		Amount: req.Amount, CreatedAt: s.clock.Now(),
	}
	switch {
	case reject:
		rf.Reason = "the refund was refused"
	case a.Status != cardsim.Captured:
		rf.Reason = "only a captured authorization can be refunded"
	case req.Amount.Currency() != a.Captured.Currency():
		rf.Reason = fmt.Sprintf("the authorization is in %s", a.Captured.Currency())
	case req.Amount.Amount() > a.Captured.Amount()-auth.refunded:
		rf.Reason = fmt.Sprintf("only %d of the capture is left to refund", a.Captured.Amount()-auth.refunded)
	default:
		rf.Status = cardsim.RefundCompleted
		auth.refunded += req.Amount.Amount()
	}
	s.refunds[key] = &refundRecord{req: *req, refund: rf}
	return rf, nil
}

func (s *Simulator) getRefund(w http.ResponseWriter, r *http.Request) {
	fault := s.faults.Next(cardsim.OpGetRefund)
	if !simkit.Before(w, r, &fault) {
		return
	}
	simkit.After(r, &fault)
	s.mu.Lock()
	rec, ok := s.refunds[r.PathValue("key")]
	var rf cardsim.Refund
	if ok {
		rf = rec.refund
	}
	s.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeRefundNotFound, "no refund with this idempotency key"))
		return
	}
	simkit.WriteJSON(w, http.StatusOK, rf)
}

func validateAuthorize(req *cardsim.Authorize) error {
	switch {
	case req.Amount.Currency() != money.BRL:
		return fmt.Errorf("amount must be in BRL, got %s", req.Amount.Currency())
	case req.Amount.Amount() <= 0:
		return fmt.Errorf("amount must be positive, got %s", req.Amount)
	case !strings.HasPrefix(req.Token, "tok_") || len(req.Token) > 64:
		return errors.New("token must start with tok_ and have at most 64 characters: card numbers are never accepted")
	}
	return nil
}
