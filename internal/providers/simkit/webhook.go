package simkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

const DeliveryTimeout = 5 * time.Second

type Clock interface {
	Now() time.Time
}

// Webhook keeps every event as the bytes first sent. Without a URL it keeps and sends
// nothing.
type Webhook struct {
	url    string
	secret []byte
	header string
	clock  Clock
	http   *http.Client

	mu     sync.Mutex
	events map[string][]byte
}

func NewWebhook(url string, secret []byte, header string, clock Clock) (*Webhook, error) {
	if url != "" && len(secret) == 0 {
		return nil, errors.New("simulator: a webhook URL needs a webhook secret")
	}
	return &Webhook{
		url: url, secret: secret, header: header, clock: clock,
		http: &http.Client{Timeout: DeliveryTimeout}, events: map[string][]byte{},
	}, nil
}

// Record keeps the bytes, so a redelivery sends the same ones.
func (h *Webhook) Record(id string, event any) []byte {
	if h.url == "" {
		return nil
	}
	body, err := json.Marshal(event)
	if err != nil {
		panic(fmt.Sprintf("simulator: encode event: %v", err)) // only plain fields: cannot fail
	}
	h.mu.Lock()
	h.events[id] = body
	h.mu.Unlock()
	return body
}

// Deliver counts any 2xx.
func (h *Webhook) Deliver(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(h.header, Sign(h.secret, h.clock.Now(), body))
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxRequestBody))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook answered %d", resp.StatusCode)
	}
	return nil
}

// Redeliver is a provider that thinks the first delivery was lost.
func (h *Webhook) Redeliver(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	body, ok := h.events[r.PathValue("id")]
	h.mu.Unlock()
	if !ok {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusNotFound, CodeEventNotFound, "no event with this ID"))
		return
	}
	if err := h.Deliver(r.Context(), body); err != nil {
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusBadGateway, CodeDeliveryFailed, err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
