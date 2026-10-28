package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	idempotencyadapters "github.com/gengibrepower/cross-border-pay/internal/idempotency/adapters"
	idempotencyapp "github.com/gengibrepower/cross-border-pay/internal/idempotency/app"
	idempotency "github.com/gengibrepower/cross-border-pay/internal/idempotency/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

type Refunds interface {
	Refund(ctx context.Context, held *idempotency.Held, merchant domain.MerchantID, payment domain.PaymentID, amount money.Money) (domain.Refund, error)
}

type refundRequest struct {
	Amount *money.Money `json:"amount"`
}

type refundResponse struct {
	ID          string       `json:"id"`
	PaymentID   string       `json:"payment_id"`
	Status      refundStatus `json:"status"`
	Amount      money.Money  `json:"amount"`
	PayerAmount money.Money  `json:"payer_amount"`
	CreatedAt   time.Time    `json:"created_at"`
}

// refundStatus hides the refund's internal statuses, as pixResponse does the attempt's:
// a merchant only needs to know whether the money went back, will not, or is on its way.
type refundStatus string

const (
	refundPending   refundStatus = "pending"
	refundSucceeded refundStatus = "succeeded"
	refundFailed    refundStatus = "failed"
)

func newRefundResponse(r *domain.Refund) refundResponse {
	status := refundPending
	switch r.Status() {
	case domain.RefundSucceeded:
		status = refundSucceeded
	case domain.RefundRejected:
		status = refundFailed
	case domain.RefundCreated, domain.RefundSent, domain.RefundUnknown:
	}
	amounts := r.Amounts()
	return refundResponse{
		ID:          r.ID().String(),
		PaymentID:   r.Payment().String(),
		Status:      status,
		Amount:      amounts.Amount,
		PayerAmount: amounts.PayerAmount,
		CreatedAt:   r.CreatedAt(),
	}
}

// refund answers 201 whatever the provider said: the refund exists, and its status says
// whether the money went back. A failed refund is an outcome, not an error of the
// request, so a retry with the same key replays it.
func (h *Handler) refund(w http.ResponseWriter, r *http.Request) {
	merchant, ok := h.authenticated(w, r)
	if !ok {
		return
	}
	held, ok := idempotencyadapters.HeldFrom(r.Context())
	if !ok {
		httpapi.WriteInternalError(w, r, h.logger, errors.New("payments: refund is not behind idempotency.Require"))
		return
	}
	id, err := domain.ParsePaymentID(r.PathValue("id"))
	if err != nil {
		writeNotFound(w, r)
		return
	}
	var req refundRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.decodeError(w, r, err)
		return
	}
	if req.Amount == nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid_request", "amount is required")
		return
	}

	refund, err := h.refunds.Refund(r.Context(), held, merchant, id, *req.Amount)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, newRefundResponse(&refund))
	case errors.Is(err, app.ErrPaymentNotFound):
		writeNotFound(w, r)
	case errors.Is(err, app.ErrPaymentNotRefundable):
		writeProblem(w, r, http.StatusConflict, "payment_not_refundable", "only a paid payment that is not fully refunded can be refunded")
	case errors.Is(err, domain.ErrRefundExceedsPayment):
		writeProblem(w, r, http.StatusUnprocessableEntity, "refund_exceeds_payment", "the refunds of this payment would exceed what was paid")
	case errors.Is(err, domain.ErrRefundTooSmall):
		writeProblem(w, r, http.StatusUnprocessableEntity, "refund_too_small", "the refund rounds to nothing in the payer's currency")
	case errors.Is(err, money.ErrCurrencyMismatch):
		writeProblem(w, r, http.StatusUnprocessableEntity, "unsupported_currency", "a refund is in the payment's currency")
	case errors.Is(err, domain.ErrInvalidAmount):
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_amount", "amount.value must be positive")
	case errors.Is(err, idempotencyapp.ErrLeaseLost):
		writeProblem(w, r, http.StatusConflict, "idempotency_key_in_progress", "a request with this Idempotency-Key is still in progress; retry later")
	case errors.Is(err, fx.ErrRateUnavailable):
		h.logger.ErrorContext(r.Context(), "no exchange rate", "error", err, "request_id", httpapi.RequestID(r.Context()))
		writeProblem(w, r, http.StatusServiceUnavailable, "rate_unavailable", "no exchange rate is available right now")
	default:
		httpapi.WriteInternalError(w, r, h.logger, err)
	}
}
