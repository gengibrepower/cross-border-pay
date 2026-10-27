package adapters

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	events "github.com/gengibrepower/cross-border-pay/internal/events/domain"
	merchantsadapters "github.com/gengibrepower/cross-border-pay/internal/merchants/adapters"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/platform/publicid"
	"github.com/gengibrepower/cross-border-pay/internal/webhooks/app"
)

const eventPrefix = "evt_"

// EventsHandler serves GET /v1/events and POST /v1/events/{id}/redeliver.
type EventsHandler struct {
	events *app.Events
	logger *slog.Logger
}

func NewEventsHandler(service *app.Events, logger *slog.Logger) *EventsHandler {
	return &EventsHandler{events: service, logger: logger}
}

func (h *EventsHandler) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/events", auth(http.HandlerFunc(h.list)))
	mux.Handle("POST /v1/events/{id}/redeliver", auth(http.HandlerFunc(h.redeliver)))
}

// eventResponse carries the event as the bytes every delivery sends, so the merchant
// sees exactly the body it would receive, next to where its delivery stands.
type eventResponse struct {
	Event    json.RawMessage  `json:"event"`
	Delivery deliveryResponse `json:"delivery"`
}

type deliveryResponse struct {
	Status         events.DeliveryStatus `json:"status"`
	Attempts       int                   `json:"attempts"`
	NextAttemptAt  *time.Time            `json:"next_attempt_at"`
	LastStatusCode *int                  `json:"last_status_code"`
	LastError      *string               `json:"last_error"`
	FinishedAt     *time.Time            `json:"finished_at"`
}

type listResponse struct {
	Data []eventResponse `json:"data"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"next_cursor"`
}

func newEventResponse(l *events.Listed) eventResponse {
	d := deliveryResponse{
		Status: l.Status, Attempts: l.Attempts,
		NextAttemptAt: optionalTime(l.NextAttemptAt), FinishedAt: optionalTime(l.FinishedAt),
	}
	if l.LastStatusCode != 0 {
		d.LastStatusCode = &l.LastStatusCode
	}
	if l.LastError != "" {
		d.LastError = &l.LastError
	}
	return eventResponse{Event: l.Event.Payload, Delivery: d}
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (h *EventsHandler) list(w http.ResponseWriter, r *http.Request) {
	principal, ok := merchantsadapters.PrincipalFrom(r.Context())
	if !ok {
		httpapi.WriteInternalError(w, r, h.logger, errors.New("webhooks: events are not behind RequireAPIKey"))
		return
	}
	query := r.URL.Query()
	q := events.ListQuery{Merchant: principal.MerchantID, Limit: app.DefaultPageSize}
	if s := query.Get("limit"); s != "" {
		limit, err := strconv.Atoi(s)
		if err != nil || limit < 1 || limit > app.MaxPageSize {
			writeProblem(w, r, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 to 100")
			return
		}
		q.Limit = limit
	}
	if s := query.Get("delivery_status"); s != "" {
		status, ok := events.ParseDeliveryStatus(s)
		if !ok {
			writeProblem(w, r, http.StatusBadRequest, "invalid_delivery_status", "delivery_status must be pending, delivered or failed")
			return
		}
		q.Status = status
	}
	if s := query.Get("cursor"); s != "" {
		before, ok := decodeCursor(s)
		if !ok {
			writeProblem(w, r, http.StatusBadRequest, "invalid_cursor", "cursor must be a next_cursor returned by this endpoint")
			return
		}
		q.Before = before
	}

	page, err := h.events.List(r.Context(), q)
	if err != nil {
		httpapi.WriteInternalError(w, r, h.logger, err)
		return
	}
	resp := listResponse{Data: make([]eventResponse, len(page.Events))}
	for i := range page.Events {
		resp.Data[i] = newEventResponse(&page.Events[i])
	}
	if page.More {
		cursor := encodeCursor(page.Events[len(page.Events)-1].Event.ID)
		resp.NextCursor = &cursor
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *EventsHandler) redeliver(w http.ResponseWriter, r *http.Request) {
	principal, ok := merchantsadapters.PrincipalFrom(r.Context())
	if !ok {
		httpapi.WriteInternalError(w, r, h.logger, errors.New("webhooks: events are not behind RequireAPIKey"))
		return
	}
	// A malformed ID gets the same 404 as another merchant's: neither exists here.
	id, ok := publicid.Parse(eventPrefix, r.PathValue("id"))
	if !ok {
		writeEventNotFound(w, r)
		return
	}
	l, err := h.events.Redeliver(r.Context(), principal.MerchantID, id)
	switch {
	case errors.Is(err, events.ErrOutgoingNotFound):
		writeEventNotFound(w, r)
	case errors.Is(err, app.ErrEndpointMissing):
		writeProblem(w, r, http.StatusConflict, "webhook_endpoint_not_found", "register a webhook endpoint with PUT /v1/webhook-endpoint first")
	case errors.Is(err, app.ErrEndpointDisabled):
		writeProblem(w, r, http.StatusConflict, "webhook_endpoint_disabled",
			"the webhook endpoint is disabled after only failures; fix it and PUT /v1/webhook-endpoint to turn it back on first")
	case err != nil:
		httpapi.WriteInternalError(w, r, h.logger, err)
	default:
		writeJSON(w, http.StatusAccepted, newEventResponse(&l))
	}
}

func writeEventNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusNotFound, "event_not_found", "no event with this id")
}

// The cursor is opaque to clients so that what it holds can change without breaking
// them; today it is the last event ID of the page.
func encodeCursor(last [16]byte) string {
	return base64.RawURLEncoding.EncodeToString(last[:])
}

func decodeCursor(s string) ([16]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 16 {
		return [16]byte{}, false
	}
	return [16]byte(b), true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	httpapi.WriteProblem(w, r, httpapi.NewProblem(status, code, detail))
}
