package adapters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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

const maxBodyBytes = 1 << 20

type Payments interface {
	Create(ctx context.Context, held *idempotency.Held, merchant domain.MerchantID, method string, amount money.Money, reference string) (app.Priced, error)
	Get(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID) (app.Priced, error)
	List(ctx context.Context, merchant domain.MerchantID, q app.ListQuery) (app.Page, error)
	Cancel(ctx context.Context, merchant domain.MerchantID, id domain.PaymentID) (app.Priced, error)
}

// MerchantFunc returns the authenticated merchant; cmd/api wires it to the merchants
// module so that payments does not import it.
type MerchantFunc func(ctx context.Context) (domain.MerchantID, bool)

type Handler struct {
	payments Payments
	merchant MerchantFunc
	logger   *slog.Logger
}

func NewHandler(payments Payments, merchant MerchantFunc, logger *slog.Logger) *Handler {
	return &Handler{payments: payments, merchant: merchant, logger: logger}
}

// Register adds the payment routes, each wrapped by auth; the ones that create or move
// money are idempotent too, inside auth. Cancel moves no money and is idempotent by
// itself: canceling twice leaves the payment as the first cancel did.
func (h *Handler) Register(mux *http.ServeMux, auth, idempotent func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/payments", auth(idempotent(http.HandlerFunc(h.create))))
	mux.Handle("GET /v1/payments", auth(http.HandlerFunc(h.list)))
	mux.Handle("GET /v1/payments/{id}", auth(http.HandlerFunc(h.get)))
	mux.Handle("POST /v1/payments/{id}/cancel", auth(http.HandlerFunc(h.cancel)))
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
	Pix       *pixResponse  `json:"pix,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

// pixResponse shows the attempt without its internal statuses: a merchant only needs
// to know whether there is a code to show, one is coming, or there will be none.
type pixResponse struct {
	Status pixStatus `json:"status"`
	BRCode string    `json:"br_code,omitempty"`
}

type pixStatus string

const (
	pixReady      pixStatus = "ready"
	pixProcessing pixStatus = "processing"
	pixFailed     pixStatus = "failed"
)

func newPixResponse(a *domain.PaymentAttempt) *pixResponse {
	if a == nil {
		return nil
	}
	switch a.Status() {
	case domain.AttemptAccepted, domain.AttemptSucceeded:
		if a.BRCode() != "" {
			return &pixResponse{Status: pixReady, BRCode: a.BRCode()}
		}
		return &pixResponse{Status: pixProcessing}
	case domain.AttemptRejected:
		return &pixResponse{Status: pixFailed}
	case domain.AttemptCreated, domain.AttemptSent, domain.AttemptUnknown:
		return &pixResponse{Status: pixProcessing}
	default:
		return &pixResponse{Status: pixProcessing}
	}
}

type listResponse struct {
	Data []paymentResponse `json:"data"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"next_cursor"`
}

func (h *Handler) authenticated(w http.ResponseWriter, r *http.Request) (domain.MerchantID, bool) {
	merchant, ok := h.merchant(r.Context())
	if !ok {
		httpapi.WriteInternalError(w, r, h.logger, errors.New("payments: route is not behind RequireAPIKey"))
	}
	return merchant, ok
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	merchant, ok := h.authenticated(w, r)
	if !ok {
		return
	}
	held, ok := idempotencyadapters.HeldFrom(r.Context())
	if !ok {
		httpapi.WriteInternalError(w, r, h.logger, errors.New("payments: create is not behind idempotency.Require"))
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

	p, err := h.payments.Create(r.Context(), held, merchant, req.Method, *req.Amount, req.Reference)
	if err != nil {
		h.createError(w, r, err)
		return
	}

	w.Header().Set("Location", "/v1/payments/"+p.Payment.ID().String())
	writeJSON(w, http.StatusCreated, newPaymentResponse(&p))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	merchant, ok := h.authenticated(w, r)
	if !ok {
		return
	}
	// A malformed ID is a 404 like any other unknown one, so the answer never depends on
	// whether the payment exists.
	id, err := domain.ParsePaymentID(r.PathValue("id"))
	if err != nil {
		writeNotFound(w, r)
		return
	}
	p, err := h.payments.Get(r.Context(), merchant, id)
	if errors.Is(err, app.ErrPaymentNotFound) {
		writeNotFound(w, r)
		return
	}
	if err != nil {
		httpapi.WriteInternalError(w, r, h.logger, err)
		return
	}
	writeJSON(w, http.StatusOK, newPaymentResponse(&p))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	merchant, ok := h.authenticated(w, r)
	if !ok {
		return
	}
	id, err := domain.ParsePaymentID(r.PathValue("id"))
	if err != nil {
		writeNotFound(w, r)
		return
	}
	p, err := h.payments.Cancel(r.Context(), merchant, id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, newPaymentResponse(&p))
	case errors.Is(err, app.ErrPaymentNotFound):
		writeNotFound(w, r)
	case errors.Is(err, domain.ErrInvalidTransition):
		writeProblem(w, r, http.StatusConflict, "payment_not_cancelable", "only a pending payment can be canceled")
	case errors.Is(err, app.ErrProviderUnavailable):
		writeProblem(w, r, http.StatusServiceUnavailable, "provider_unavailable", "the provider could not confirm the payment is unpaid; nothing changed, retry later")
	default:
		httpapi.WriteInternalError(w, r, h.logger, err)
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	merchant, ok := h.authenticated(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	q := app.ListQuery{Limit: app.DefaultPageSize}
	if s := query.Get("limit"); s != "" {
		limit, err := strconv.Atoi(s)
		if err != nil {
			writeInvalidLimit(w, r)
			return
		}
		q.Limit = limit
	}
	if s := query.Get("status"); s != "" {
		status, err := domain.ParseStatus(s)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "invalid_status", "status must be one of the payment statuses")
			return
		}
		q.Status = status
	}
	if s := query.Get("cursor"); s != "" {
		before, err := decodeCursor(s)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "invalid_cursor", "cursor must be a next_cursor returned by this endpoint")
			return
		}
		q.Before = before
	}

	page, err := h.payments.List(r.Context(), merchant, q)
	if errors.Is(err, app.ErrInvalidLimit) {
		writeInvalidLimit(w, r)
		return
	}
	if err != nil {
		httpapi.WriteInternalError(w, r, h.logger, err)
		return
	}
	resp := listResponse{Data: make([]paymentResponse, len(page.Payments))}
	for i := range page.Payments {
		resp.Data[i] = newPaymentResponse(&page.Payments[i])
	}
	if page.More {
		cursor := encodeCursor(page.Payments[len(page.Payments)-1].Payment.ID())
		resp.NextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, resp)
}

// The cursor is opaque to clients so that what it holds can change without breaking
// them; today it is the last payment ID of the page.
func encodeCursor(last domain.PaymentID) string {
	return base64.RawURLEncoding.EncodeToString(last[:])
}

func decodeCursor(s string) (domain.PaymentID, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return domain.PaymentID{}, fmt.Errorf("payments: cursor: %w", err)
	}
	var id domain.PaymentID
	if len(b) != len(id) {
		return domain.PaymentID{}, errors.New("payments: cursor has the wrong length")
	}
	return domain.PaymentID(b), nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeInvalidLimit(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 to 100")
}

func writeNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusNotFound, "payment_not_found", "no payment with this ID")
}

func newPaymentResponse(priced *app.Priced) paymentResponse {
	p, q := &priced.Payment, &priced.Quote
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
		Pix:       newPixResponse(priced.Attempt),
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
	case errors.Is(err, idempotencyapp.ErrLeaseLost):
		writeProblem(w, r, http.StatusConflict, "idempotency_key_in_progress", "a request with this Idempotency-Key is still in progress; retry later")
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
