package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/merchants/app"
	"github.com/gengibrepower/cross-border-pay/internal/merchants/domain"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
)

const maxEndpointBodyBytes = 16 << 10

type EndpointService interface {
	Get(ctx context.Context, merchant domain.MerchantID) (domain.Endpoint, error)
	Put(ctx context.Context, merchant domain.MerchantID, c domain.Change) (domain.Endpoint, string, error)
}

type EndpointHandler struct {
	endpoints EndpointService
	logger    *slog.Logger
}

func NewEndpointHandler(endpoints EndpointService, logger *slog.Logger) *EndpointHandler {
	return &EndpointHandler{endpoints: endpoints, logger: logger}
}

func (h *EndpointHandler) Register(mux *http.ServeMux, auth func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/webhook-endpoint", auth(http.HandlerFunc(h.get)))
	mux.Handle("PUT /v1/webhook-endpoint", auth(http.HandlerFunc(h.put)))
}

type putEndpointRequest struct {
	URL              *string `json:"url"`
	RotateSecretFrom *int    `json:"rotate_secret_from"`
}

type endpointResponse struct {
	URL           string `json:"url"`
	SecretVersion int    `json:"secret_version"`
	// Secret is present only on the first registration and on a rotation.
	Secret string `json:"secret,omitempty"`
	// Decision 24: set while deliveries are stopped; any PUT turns them back on.
	FailingSince   *time.Time `json:"failing_since,omitempty"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func newEndpointResponse(e *domain.Endpoint, secret string) endpointResponse {
	return endpointResponse{
		URL: e.URL, SecretVersion: int(e.Version), Secret: secret,
		FailingSince: optionalTime(e.FailingSince), DisabledAt: optionalTime(e.DisabledAt), DisabledReason: e.DisabledReason,
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (h *EndpointHandler) get(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.WriteInternalError(w, r, h.logger, errors.New("merchants: webhook endpoint is not behind RequireAPIKey"))
		return
	}
	e, err := h.endpoints.Get(r.Context(), principal.MerchantID)
	switch {
	case errors.Is(err, app.ErrEndpointNotFound):
		writeProblem(w, r, http.StatusNotFound, "webhook_endpoint_not_found", "no webhook endpoint is registered; PUT one first")
	case err != nil:
		httpapi.WriteInternalError(w, r, h.logger, err)
	default:
		w.Header().Set("Content-Type", "application/json")
		//nolint:gosec // G117: no secret here; an empty one is dropped by omitempty.
		_ = json.NewEncoder(w).Encode(newEndpointResponse(&e, ""))
	}
}

func (h *EndpointHandler) put(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.WriteInternalError(w, r, h.logger, errors.New("merchants: webhook endpoint is not behind RequireAPIKey"))
		return
	}
	var req putEndpointRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEndpointBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.URL == nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid_request", "the body must be {\"url\": \"https://...\"}, with an optional rotate_secret_from")
		return
	}
	c := domain.Change{URL: *req.URL}
	if req.RotateSecretFrom != nil {
		if *req.RotateSecretFrom < 1 {
			writeProblem(w, r, http.StatusBadRequest, "invalid_request", "rotate_secret_from must be a secret_version this endpoint returned")
			return
		}
		c.RotateFrom = domain.SecretVersion(*req.RotateSecretFrom)
	}

	e, secret, err := h.endpoints.Put(r.Context(), principal.MerchantID, c)
	switch {
	case errors.Is(err, ssrf.ErrInvalidURL), errors.Is(err, ssrf.ErrForbiddenURL):
		writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_webhook_url",
			"url must be an https URL of at most 2048 characters, without credentials, on the public internet")
	case errors.Is(err, domain.ErrSecretVersionConflict):
		writeProblem(w, r, http.StatusConflict, "secret_version_conflict",
			"rotate_secret_from must be the endpoint's current secret_version")
	case err != nil:
		httpapi.WriteInternalError(w, r, h.logger, err)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		//nolint:gosec // G117: the one answer meant to carry the secret, which is never stored or logged.
		_ = json.NewEncoder(w).Encode(newEndpointResponse(&e, secret))
	}
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	httpapi.WriteProblem(w, r, httpapi.NewProblem(status, code, detail))
}
