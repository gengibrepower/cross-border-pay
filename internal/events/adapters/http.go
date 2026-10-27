package adapters

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gengibrepower/cross-border-pay/internal/events/app"
	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

// maxEventBytes is far above any provider event; a bigger body is not one.
const maxEventBytes = 64 << 10

type Receiver interface {
	Receive(ctx context.Context, provider string, header func(string) string, body []byte) error
}

type Handler struct {
	inbox  Receiver
	logger *slog.Logger
}

func NewHandler(inbox Receiver, logger *slog.Logger) *Handler {
	return &Handler{inbox: inbox, logger: logger}
}

// Register mounts the providers' webhook. It carries no API key: the signature is the
// authentication, checked by the provider's verifier.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/providers/{provider}/events", h.receive)
}

// receive answers 200 for a stored event and for a duplicate alike: either way the
// provider must stop delivering it. Only a failure to store asks for a redelivery.
func (h *Handler) receive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "the body is larger than 64 KiB")
			return
		}
		problem(w, r, http.StatusBadRequest, "invalid_request", "the body could not be read")
		return
	}

	err = h.inbox.Receive(r.Context(), r.PathValue("provider"), r.Header.Get, body)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"received"}`)
	case errors.Is(err, app.ErrUnknownProvider):
		problem(w, r, http.StatusNotFound, "unknown_provider", "no provider sends events to this URL")
	case errors.Is(err, app.ErrUnauthenticated):
		h.logger.WarnContext(r.Context(), "provider event refused", "provider", r.PathValue("provider"), "error", err)
		problem(w, r, http.StatusUnauthorized, "invalid_signature", "the event signature is missing, wrong or too old")
	case errors.Is(err, app.ErrMalformedEvent):
		h.logger.WarnContext(r.Context(), "provider event refused", "provider", r.PathValue("provider"), "error", err)
		problem(w, r, http.StatusBadRequest, "invalid_event", "the event could not be read")
	default:
		httpapi.WriteInternalError(w, r, h.logger, err)
	}
}

func problem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	httpapi.WriteProblem(w, r, httpapi.NewProblem(status, code, detail))
}
