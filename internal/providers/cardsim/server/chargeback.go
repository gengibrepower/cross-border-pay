package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/providers/cardsim"
	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

// openChargeback never takes more than is left after earlier chargebacks. Refunds do
// not count: a refunded cardholder can still dispute (decision 7).
func (s *Simulator) openChargeback(w http.ResponseWriter, r *http.Request) {
	var req cardsim.OpenChargeback
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	reason := strings.TrimSpace(req.Reason)
	switch {
	case req.Amount.Amount() <= 0:
		simkit.Invalid(w, r, fmt.Sprintf("amount must be positive, got %s", req.Amount))
		return
	case reason == "" || utf8.RuneCountInString(reason) > cardsim.MaxReason:
		simkit.Invalid(w, r, fmt.Sprintf("reason must have 1 to %d characters", cardsim.MaxReason))
		return
	}

	s.mu.Lock()
	key, found := s.keyByID[r.PathValue("id")]
	var (
		cb      cardsim.Chargeback
		refusal string
		event   []byte
	)
	if found {
		rec := s.byKey[key]
		a := s.current(rec)
		switch {
		case a.Status != cardsim.Captured:
			refusal = fmt.Sprintf("the authorization is %s", a.Status)
		case req.Amount.Currency() != a.Captured.Currency():
			refusal = fmt.Sprintf("the authorization is in %s", a.Captured.Currency())
		case req.Amount.Amount() > a.Captured.Amount()-rec.chargedBack:
			refusal = fmt.Sprintf("only %d of the capture is left to charge back", a.Captured.Amount()-rec.chargedBack)
		default:
			rec.chargebacks++
			rec.chargedBack += req.Amount.Amount()
			name := key + "\x00" + strconv.Itoa(rec.chargebacks)
			cb = cardsim.Chargeback{
				ID:              "cbk_" + simkit.Text(s.seed.RNG("chargeback\x00"+name), idLength),
				AuthorizationID: a.ID, AuthorizationKey: key, Status: cardsim.NeedsResponse,
				Amount: req.Amount, Reason: reason, CreatedAt: s.clock.Now(),
			}
			s.chargebacks[cb.ID] = &cb
			event = s.record(cardsim.ChargebackOpened, &cb)
		}
	}
	s.mu.Unlock()

	switch {
	case !found:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeNotFound, "no authorization with this ID"))
	case refusal != "":
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, cardsim.CodeNotDisputable, refusal))
	default:
		s.deliver(r, cb.ID, event)
		simkit.WriteJSON(w, http.StatusCreated, cb)
	}
}

// record: the caller holds s.mu. The ID comes from the chargeback and the event's
// type, so each fact has one.
func (s *Simulator) record(t cardsim.EventType, cb *cardsim.Chargeback) []byte {
	e := cardsim.Event{
		ID:   "evt_" + simkit.Text(s.seed.RNG(string(t)+"\x00"+cb.ID), idLength),
		Type: t, CreatedAt: s.clock.Now(), Chargeback: *cb,
	}
	return s.webhook.Record(e.ID, &e)
}

// deliver: a client that gives up must not cut it short, and a failed delivery does
// not undo the fact.
func (s *Simulator) deliver(r *http.Request, chargeback string, event []byte) {
	if event == nil {
		return
	}
	if err := s.webhook.Deliver(context.WithoutCancel(r.Context()), event); err != nil {
		s.logger.Warn("event delivery failed", "chargeback", chargeback, "error", err)
	}
}

func (s *Simulator) getChargeback(w http.ResponseWriter, r *http.Request) {
	fault := s.faults.Next(cardsim.OpGetChargeback)
	if !simkit.Before(w, r, &fault) {
		return
	}
	simkit.After(r, &fault)
	s.mu.Lock()
	cb, ok := s.chargebacks[r.PathValue("id")]
	var out cardsim.Chargeback
	if ok {
		out = *cb
	}
	s.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeChargebackNotFound, "no chargeback with this ID"))
		return
	}
	simkit.WriteJSON(w, http.StatusOK, out)
}

func (s *Simulator) accept(w http.ResponseWriter, r *http.Request) {
	s.respond(w, r, func(cb *cardsim.Chargeback) (bool, string) {
		switch cb.Status {
		case cardsim.NeedsResponse:
			return true, ""
		case cardsim.Lost:
			return false, ""
		case cardsim.UnderReview, cardsim.Won:
			return false, fmt.Sprintf("the chargeback is %s", cb.Status)
		default:
			return false, fmt.Sprintf("the chargeback is %s", cb.Status)
		}
	}, cardsim.Lost)
}

func (s *Simulator) contest(w http.ResponseWriter, r *http.Request) {
	var req cardsim.Contest
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(req.Evidence)); n < 1 || n > cardsim.MaxEvidence {
		simkit.Invalid(w, r, fmt.Sprintf("evidence must have 1 to %d characters", cardsim.MaxEvidence))
		return
	}
	s.respond(w, r, func(cb *cardsim.Chargeback) (bool, string) {
		switch cb.Status {
		case cardsim.NeedsResponse:
			return true, ""
		case cardsim.UnderReview:
			return false, ""
		case cardsim.Won, cardsim.Lost:
			return false, fmt.Sprintf("the chargeback is %s", cb.Status)
		default:
			return false, fmt.Sprintf("the chargeback is %s", cb.Status)
		}
	}, cardsim.UnderReview)
}

func (s *Simulator) respond(w http.ResponseWriter, r *http.Request, decide func(*cardsim.Chargeback) (move bool, refusal string), status cardsim.ChargebackStatus) {
	fault := s.faults.Next(cardsim.OpRespond)
	if !simkit.Before(w, r, &fault) {
		return
	}
	s.mu.Lock()
	cb, found := s.chargebacks[r.PathValue("id")]
	var (
		out     cardsim.Chargeback
		refusal string
		event   []byte
	)
	if found {
		var move bool
		move, refusal = decide(cb)
		if move {
			event = s.move(cb, status)
		}
		out = *cb
	}
	s.mu.Unlock()
	simkit.After(r, &fault)

	switch {
	case !found:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeChargebackNotFound, "no chargeback with this ID"))
	case refusal != "":
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, cardsim.CodeChargebackClosed, refusal))
	default:
		s.deliver(r, out.ID, event)
		simkit.WriteJSON(w, http.StatusOK, out)
	}
}

// move: the caller holds s.mu.
func (s *Simulator) move(cb *cardsim.Chargeback, status cardsim.ChargebackStatus) []byte {
	cb.Status = status
	if status != cardsim.Won && status != cardsim.Lost {
		return nil
	}
	now := s.clock.Now()
	cb.ClosedAt = &now
	return s.record(cardsim.ChargebackClosed, cb)
}

// resolve plays the issuer. The same outcome again returns the chargeback as it is.
func (s *Simulator) resolve(w http.ResponseWriter, r *http.Request) {
	var req cardsim.ResolveChargeback
	if err := simkit.Decode(w, r, &req); err != nil {
		simkit.Invalid(w, r, err.Error())
		return
	}
	if req.Outcome != cardsim.Won && req.Outcome != cardsim.Lost {
		simkit.Invalid(w, r, fmt.Sprintf("outcome must be won or lost, got %q", req.Outcome))
		return
	}
	s.mu.Lock()
	cb, found := s.chargebacks[r.PathValue("id")]
	var (
		out     cardsim.Chargeback
		refusal string
		event   []byte
	)
	if found {
		switch {
		case cb.Status == req.Outcome:
		case cb.Status == cardsim.UnderReview, cb.Status == cardsim.NeedsResponse && req.Outcome == cardsim.Lost:
			event = s.move(cb, req.Outcome)
		default:
			refusal = fmt.Sprintf("a chargeback %s cannot be %s", cb.Status, req.Outcome)
		}
		out = *cb
	}
	s.mu.Unlock()

	switch {
	case !found:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, cardsim.CodeChargebackNotFound, "no chargeback with this ID"))
	case refusal != "":
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusConflict, cardsim.CodeChargebackClosed, refusal))
	default:
		s.deliver(r, out.ID, event)
		simkit.WriteJSON(w, http.StatusOK, out)
	}
}
