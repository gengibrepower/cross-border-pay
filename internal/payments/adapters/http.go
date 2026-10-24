package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	fx "github.com/gengibrepower/cross-border-pay/internal/fx/domain"
	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

const maxBodyBytes = 1 << 20

type Creator interface {
	Create(ctx context.Context, merchant domain.MerchantID, method string, amount money.Money, reference string) (domain.Payment, fx.Quote, error)
}

// MerchantFunc returns the authenticated merchant; cmd/api wires it to the merchants
// module so that payments does not import it.
type MerchantFunc func(ctx context.Context) (domain.MerchantID, bool)

type Handler struct {
	payments Creator
	merchant MerchantFunc
	logger   *slog.Logger
}

func NewHandler(payments Creator, merchant MerchantFunc, logger *slog.Logger) *Handler {
	return &Handler{payments: payments, merchant: merchant, logger: logger}
}

// Register adds the payment routes, each wrapped by auth.
func (h *Handler) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/payments", auth(http.HandlerFunc(h.create)))
}

type createRequest struct {
	Amount    *money.Money `json:"amount"`
	Method    string       `json:"method"`
	Reference string       `json:"reference"`
}

type quoteResponse struct {
	PayerAmount money.Money `json:"payer_amount"`
	BaseRate    string      `json:"base_rate"`
	Fee         money.Money `json:"fee"`
	NetAmount   money.Money `json:"net_amount"`
	ExpiresAt   time.Time   `json:"expires_at"`
}

type paymentResponse struct {
	ID        string        `json:"id"`
	Status    domain.Status `json:"status"`
	Method    domain.Method `json:"method"`
	Amount    money.Money   `json:"amount"`
	Reference string        `json:"reference"`
	Quote     quoteResponse `json:"quote"`
	CreatedAt time.Time     `json:"created_at"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	merchant, ok := h.merchant(r.Context())
	if !ok {
		httpapi.WriteInternalError(w, r, h.logger, errors.New("payments: route is not behind RequireAPIKey"))
		return
	}
	// Stored and enforced in F4; required now so the contract does not change then.
	if key := r.Header.Get("Idempotency-Key"); key == "" || len(key) > 255 {
		writeProblem(w, r, http.StatusBadRequest, "idempotency_key_required", "send an Idempotency-Key header of 1 to 255 characters")
		return
	}

	var req createRequest
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

	payment, quote, err := h.payments.Create(r.Context(), merchant, req.Method, *req.Amount, req.Reference)
	if err != nil {
		h.createError(w, r, err)
		return
	}

	w.Header().Set("Location", "/v1/payments/"+payment.ID().String())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(newPaymentResponse(&payment, &quote))
}

func newPaymentResponse(p *domain.Payment, q *fx.Quote) paymentResponse {
	return paymentResponse{
		ID:        p.ID().String(),
		Status:    p.Status(),
		Method:    p.Method(),
		Amount:    p.Amount(),
		Reference: p.Reference(),
		Quote: quoteResponse{
			PayerAmount: q.PayerAmount(),
			BaseRate:    q.BaseRate().Decimal(),
			Fee:         q.Fee(),
			NetAmount:   q.MerchantNet(),
			ExpiresAt:   q.ExpiresAt(),
		},
		CreatedAt: p.CreatedAt(),
	}
}

func (h *Handler) decodeError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "the body is larger than 1 MiB")
	case errors.Is(err, money.ErrInvalidAmount):
		writeProblem(w, r, http.StatusBadRequest, "invalid_amount", "amount.value must be an integer in minor units, e.g. 2000 for US$ 20.00")
	case errors.Is(err, money.ErrUnknownCurrency):
		writeProblem(w, r, http.StatusBadRequest, "invalid_currency", "amount.currency must be an ISO 4217 code this API knows")
	default:
		writeProblem(w, r, http.StatusBadRequest, "invalid_request", "the body is not a valid payment request")
	}
}

func (h *Handler) createError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrUnsupportedMethod):
		writeProblem(w, r, http.StatusUnprocessableEntity, "unsupported_method", "only pix is available for now")
	case errors.Is(err, domain.ErrUnknownMethod):
		writeProblem(w, r, http.StatusBadRequest, "invalid_method", "method must be pix, boleto or card")
	case errors.Is(err, domain.ErrUnsupportedCurrency):
		writeProblem(w, r, http.StatusUnprocessableEntity, "unsupported_currency", "payments are charged in USD")
	case errors.Is(err, domain.ErrInvalidAmount):
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_amount", "amount.value must be positive")
	case errors.Is(err, domain.ErrInvalidReference):
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_reference", "reference must have 1 to 100 characters")
	case errors.Is(err, fx.ErrRateUnavailable):
		h.logger.ErrorContext(r.Context(), "no exchange rate", "error", err, "request_id", httpapi.RequestID(r.Context()))
		writeProblem(w, r, http.StatusServiceUnavailable, "rate_unavailable", "no exchange rate is available right now")
	default:
		httpapi.WriteInternalError(w, r, h.logger, err)
	}
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	httpapi.WriteProblem(w, r, httpapi.NewProblem(status, code, detail))
}
