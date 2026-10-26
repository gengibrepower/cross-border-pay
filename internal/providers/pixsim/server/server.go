// Package server is the PIX simulator: a provider that keeps its charges in memory,
// derives every random choice from a seed and fails on command.
package server

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
}

type Clock interface {
	Now() time.Time
}

type record struct {
	req    pixsim.CreateCharge
	charge pixsim.Charge
}

type Simulator struct {
	cfg   Config
	clock Clock

	mu      sync.Mutex
	byKey   map[string]*record
	keyByID map[string]string
	faults  []pixsim.Fault
}

func New(cfg Config, clock Clock) (*Simulator, error) {
	probe := brcode.Code{Key: cfg.PixKey, MerchantName: cfg.MerchantName, MerchantCity: cfg.MerchantCity}
	if _, err := brcode.Encode(&probe); err != nil {
		return nil, fmt.Errorf("pixsim: receiver: %w", err)
	}
	return &Simulator{cfg: cfg, clock: clock, byKey: map[string]*record{}, keyByID: map[string]string{}}, nil
}

func (s *Simulator) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/charges", s.create)
	mux.HandleFunc("GET /v1/charges/by-key/{key}", s.get)
	mux.HandleFunc("POST /control/charges/{id}/pay", s.pay)
	mux.HandleFunc("POST /control/faults", s.injectFault)
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

// pay plays the payer. A static PIX code can be paid at any time, so a charge past
// its expiry is paid too: that is the late payment of decision 8.
func (s *Simulator) pay(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	key, ok := s.keyByID[r.PathValue("id")]
	var ch pixsim.Charge
	if ok {
		rec := s.byKey[key]
		if rec.charge.Status == pixsim.Active {
			paidAt := s.clock.Now()
			rec.charge.Status = pixsim.Paid
			rec.charge.PaidAt = &paidAt
		}
		ch = rec.charge
	}
	s.mu.Unlock()

	switch {
	case !ok:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, pixsim.CodeNotFound, "no charge with this ID"))
	case ch.Status == pixsim.Rejected:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, pixsim.CodeNotPayable, "a rejected charge cannot be paid"))
	default:
		writeJSON(w, http.StatusOK, ch)
	}
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
	case pixsim.OpCreate, pixsim.OpGet:
	default:
		return fmt.Errorf("op must be create or get, got %q", f.Op)
	}
	switch f.Kind {
	case pixsim.Reject:
		if f.Op != pixsim.OpCreate {
			return errors.New("only a create can be rejected")
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
