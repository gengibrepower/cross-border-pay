package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

type Chargebacks interface {
	Get(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID) (domain.ChargebackCase, error)
	Accept(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID) (domain.ChargebackCase, error)
	Contest(ctx context.Context, merchant domain.MerchantID, id domain.ChargebackID, evidence string) (domain.ChargebackCase, error)
}

// chargebackResponse is a chargeback as the merchant sees it: what the bank took back
// from the payer, what the merchant lost with it, and where the dispute is.
type chargebackResponse struct {
	ID          string                  `json:"id"`
	PaymentID   string                  `json:"payment_id"`
	Status      domain.ChargebackStatus `json:"status"`
	Reason      string                  `json:"reason"`
	PayerAmount money.Money             `json:"payer_amount"`
	Amount      money.Money             `json:"amount"`
	Evidence    *string                 `json:"evidence"`
	CreatedAt   time.Time               `json:"created_at"`
	ClosedAt    *time.Time              `json:"closed_at"`
}

func newChargebackResponse(c *domain.ChargebackCase) chargebackResponse {
	resp := chargebackResponse{
		ID: c.ID.String(), PaymentID: c.Payment.String(), Status: c.Status, Reason: c.Reason,
		PayerAmount: c.Amounts.PayerAmount, Amount: c.Amounts.Amount, CreatedAt: c.CreatedAt,
	}
	if c.Evidence != "" {
		ev := c.Evidence
		resp.Evidence = &ev
	}
	if !c.ClosedAt.IsZero() {
		at := c.ClosedAt
		resp.ClosedAt = &at
	}
	return resp
}

func (h *Handler) chargebackID(w http.ResponseWriter, r *http.Request) (domain.MerchantID, domain.ChargebackID, bool) {
	merchant, ok := h.authenticated(w, r)
	if !ok {
		return domain.MerchantID{}, domain.ChargebackID{}, false
	}
	id, err := domain.ParseChargebackID(r.PathValue("id"))
	if err != nil {
		writeNotFound(w, r)
		return domain.MerchantID{}, domain.ChargebackID{}, false
	}
	return merchant, id, true
}

func (h *Handler) getChargeback(w http.ResponseWriter, r *http.Request) {
	merchant, id, ok := h.chargebackID(w, r)
	if !ok {
		return
	}
	c, err := h.chargebacks.Get(r.Context(), merchant, id)
	h.writeChargeback(w, r, &c, err)
}

// acceptChargeback needs no Idempotency-Key, as capture: accepting again returns the
// chargeback as it is.
func (h *Handler) acceptChargeback(w http.ResponseWriter, r *http.Request) {
	merchant, id, ok := h.chargebackID(w, r)
	if !ok {
		return
	}
	c, err := h.chargebacks.Accept(r.Context(), merchant, id)
	h.writeChargeback(w, r, &c, err)
}

type contestRequest struct {
	Evidence string `json:"evidence"`
}

func (h *Handler) contestChargeback(w http.ResponseWriter, r *http.Request) {
	merchant, id, ok := h.chargebackID(w, r)
	if !ok {
		return
	}
	var req contestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.decodeError(w, r, err)
		return
	}
	c, err := h.chargebacks.Contest(r.Context(), merchant, id, req.Evidence)
	h.writeChargeback(w, r, &c, err)
}

func (h *Handler) writeChargeback(w http.ResponseWriter, r *http.Request, c *domain.ChargebackCase, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, newChargebackResponse(c))
	case errors.Is(err, app.ErrChargebackNotFound):
		writeNotFound(w, r)
	case errors.Is(err, domain.ErrInvalidEvidence):
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_evidence", "evidence must have 1 to 2000 characters")
	case errors.Is(err, domain.ErrChargebackClosed):
		writeProblem(w, r, http.StatusConflict, "chargeback_closed", "the chargeback can no longer take this answer: it was answered otherwise or decided")
	case errors.Is(err, app.ErrProviderUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "provider_unavailable", "the acquirer did not answer; nothing changed here, retry later")
	default:
		httpapi.WriteInternalError(w, r, h.logger, err)
	}
}
