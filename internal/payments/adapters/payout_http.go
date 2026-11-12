package adapters

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/money"
	"github.com/gengibrepower/cross-border-pay/internal/payments/app"
	"github.com/gengibrepower/cross-border-pay/internal/payments/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

type Payouts interface {
	Balance(ctx context.Context, merchant domain.MerchantID) (domain.Balance, error)
	Settlements(ctx context.Context, merchant domain.MerchantID, q app.SettlementQuery) (app.SettlementPage, error)
}

type balanceResponse struct {
	Available money.Money `json:"available"`
	Pending   money.Money `json:"pending"`
	Reserved  money.Money `json:"reserved"`
}

type settlementResponse struct {
	ID         string      `json:"id"`
	CutoffDate string      `json:"cutoff_date"`
	Amount     money.Money `json:"amount"`
	Payments   []string    `json:"payments"`
	CreatedAt  time.Time   `json:"created_at"`
}

type settlementListResponse struct {
	Data []settlementResponse `json:"data"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"next_cursor"`
}

func (h *Handler) balance(w http.ResponseWriter, r *http.Request) {
	merchant, ok := h.authenticated(w, r)
	if !ok {
		return
	}
	b, err := h.payouts.Balance(r.Context(), merchant)
	if err != nil {
		httpapi.WriteInternalError(w, r, h.logger, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResponse{Available: b.Available, Pending: b.Pending, Reserved: b.Reserved})
}

func (h *Handler) settlements(w http.ResponseWriter, r *http.Request) {
	merchant, ok := h.authenticated(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	q := app.SettlementQuery{Limit: app.DefaultPageSize}
	if s := query.Get("limit"); s != "" {
		limit, err := strconv.Atoi(s)
		if err != nil {
			writeInvalidLimit(w, r)
			return
		}
		q.Limit = limit
	}
	if s := query.Get("cursor"); s != "" {
		before, err := decodeDateCursor(s)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "invalid_cursor", "cursor must be a next_cursor returned by this endpoint")
			return
		}
		q.Before = before
	}

	page, err := h.payouts.Settlements(r.Context(), merchant, q)
	if errors.Is(err, app.ErrInvalidLimit) {
		writeInvalidLimit(w, r)
		return
	}
	if err != nil {
		httpapi.WriteInternalError(w, r, h.logger, err)
		return
	}
	resp := settlementListResponse{Data: make([]settlementResponse, len(page.Settlements))}
	for i := range page.Settlements {
		resp.Data[i] = newSettlementResponse(&page.Settlements[i])
	}
	if page.More {
		cursor := encodeDateCursor(page.Settlements[len(page.Settlements)-1].CutoffDate)
		resp.NextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, resp)
}

func newSettlementResponse(s *domain.Settlement) settlementResponse {
	ids := make([]string, len(s.Payments))
	for i, id := range s.Payments {
		ids[i] = id.String()
	}
	return settlementResponse{
		ID:         s.ID.String(),
		CutoffDate: s.CutoffDate.Format(time.DateOnly),
		Amount:     s.Amount,
		Payments:   ids,
		CreatedAt:  s.CreatedAt,
	}
}

// The settlements cursor is the last cutoff date of the page, opaque like the payments
// one: a settlement is unique per merchant and cutoff, so the date is the key.
func encodeDateCursor(last time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte(last.Format(time.DateOnly)))
}

func decodeDateCursor(s string) (time.Time, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("payments: cursor: %w", err)
	}
	t, err := time.Parse(time.DateOnly, string(b))
	if err != nil {
		return time.Time{}, fmt.Errorf("payments: cursor: %w", err)
	}
	return t, nil
}
