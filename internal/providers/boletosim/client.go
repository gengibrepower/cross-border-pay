package boletosim

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

var (
	// ErrRejected is the bank refusing to register a boleto, for good. It is the only
	// create error that says the bank did nothing; every other one is ambiguous.
	ErrRejected = errors.New("boletosim: boleto rejected")
	ErrNotFound = errors.New("boletosim: boleto not found")
	// ErrKeyReused is the same idempotency key sent with another request.
	ErrKeyReused = errors.New("boletosim: idempotency key reused with another request")
	// ErrNotPayable is paying a rejected boleto, and ErrOverdue one past its last day
	// (decision 53).
	ErrNotPayable     = errors.New("boletosim: boleto cannot be paid")
	ErrOverdue        = errors.New("boletosim: boleto is past its due date")
	ErrRefundRejected = errors.New("boletosim: refund rejected")
	ErrRefundNotFound = errors.New("boletosim: refund not found")
	ErrEventNotFound  = errors.New("boletosim: event not found")
	ErrDeliveryFailed = errors.New("boletosim: event delivery failed")
	ErrBadResponse    = simkit.ErrBadResponse
)

type Client struct {
	c *simkit.Client
}

// NewClient talks to the simulator at baseURL. httpClient nil means one with
// simkit.DefaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	c, err := simkit.NewClient("boletosim", baseURL, httpClient, problemError)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Create registers the boleto once per key: repeating the same request returns the
// same boleto, so a create whose answer was lost can be retried or looked up with Get.
func (c *Client) Create(ctx context.Context, key string, req *CreateBoleto) (Boleto, error) {
	var b Boleto
	err := c.c.Do(ctx, http.MethodPost, "/v1/boletos", key, req, http.StatusCreated, &b)
	return b, err
}

// Get finds a boleto by the idempotency key it was created with.
func (c *Client) Get(ctx context.Context, key string) (Boleto, error) {
	var b Boleto
	err := c.c.Do(ctx, http.MethodGet, "/v1/boletos/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &b)
	return b, err
}

// Refund returns amount of the paid boleto to the payer, once per key.
func (c *Client) Refund(ctx context.Context, key, boletoID string, req CreateRefund) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodPost, "/v1/boletos/"+url.PathEscape(boletoID)+"/refunds", key, req, http.StatusCreated, &r)
	return r, err
}

// GetRefund finds a refund by the idempotency key it was created with.
func (c *Client) GetRefund(ctx context.Context, key string) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodGet, "/v1/refunds/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &r)
	return r, err
}

// Pay is the control endpoint that plays the payer at a bank branch: it pays the
// boleto now, unless its last day went by.
func (c *Client) Pay(ctx context.Context, id string) (Boleto, error) {
	var b Boleto
	err := c.c.Do(ctx, http.MethodPost, "/control/boletos/"+url.PathEscape(id)+"/pay", "", nil, http.StatusOK, &b)
	return b, err
}

// Redeliver is the control endpoint that sends an event again.
func (c *Client) Redeliver(ctx context.Context, eventID string) error {
	return c.c.Do(ctx, http.MethodPost, "/control/events/"+url.PathEscape(eventID)+"/deliver", "", nil, http.StatusNoContent, nil)
}

// InjectFault is the control endpoint that schedules failures.
func (c *Client) InjectFault(ctx context.Context, f Fault) error {
	return c.c.Do(ctx, http.MethodPost, "/control/faults", "", f, http.StatusNoContent, nil)
}

func problemError(status int, code string) error {
	switch {
	case status == http.StatusUnprocessableEntity && code == CodeRejected:
		return ErrRejected
	case status == http.StatusNotFound && code == CodeNotFound:
		return ErrNotFound
	case status == http.StatusUnprocessableEntity && code == CodeKeyReused:
		return ErrKeyReused
	case status == http.StatusConflict && code == CodeNotPayable:
		return ErrNotPayable
	case status == http.StatusConflict && code == CodeOverdue:
		return ErrOverdue
	case status == http.StatusUnprocessableEntity && code == CodeRefundRejected:
		return ErrRefundRejected
	case status == http.StatusNotFound && code == CodeRefundNotFound:
		return ErrRefundNotFound
	case status == http.StatusNotFound && code == CodeEventNotFound:
		return ErrEventNotFound
	case status == http.StatusBadGateway && code == CodeDeliveryFailed:
		return ErrDeliveryFailed
	default:
		return nil
	}
}
