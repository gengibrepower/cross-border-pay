package pixsim

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

var (
	// ErrRejected is the simulator refusing a charge for good. It is the only error
	// that says the provider did nothing; every other one is ambiguous.
	ErrRejected = errors.New("pixsim: charge rejected")
	ErrNotFound = errors.New("pixsim: charge not found")
	// ErrKeyReused is the same idempotency key sent with a different charge.
	ErrKeyReused     = errors.New("pixsim: idempotency key reused with another charge")
	ErrNotPayable    = errors.New("pixsim: charge cannot be paid")
	ErrEventNotFound = errors.New("pixsim: event not found")
	// ErrDeliveryFailed is the webhook not answering 2xx to a redelivery.
	ErrDeliveryFailed = errors.New("pixsim: event delivery failed")
	ErrBadResponse    = simkit.ErrBadResponse
	// ErrRefundRejected is the simulator refusing a refund for good; like ErrRejected,
	// the refund is stored and Get finds it rejected.
	ErrRefundRejected = errors.New("pixsim: refund rejected")
	ErrRefundNotFound = errors.New("pixsim: refund not found")
)

// DefaultTimeout bounds every call; a call that hits it is ambiguous, not failed.
const DefaultTimeout = simkit.DefaultTimeout

type Client struct {
	c *simkit.Client
}

// NewClient talks to the simulator at baseURL. httpClient nil means one with
// DefaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	c, err := simkit.NewClient("pixsim", baseURL, httpClient, problemError)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Create creates the charge once per key: repeating the same request returns the same
// charge, so a create whose answer was lost can be retried or looked up with Get.
func (c *Client) Create(ctx context.Context, key string, req CreateCharge) (Charge, error) {
	var ch Charge
	err := c.c.Do(ctx, http.MethodPost, "/v1/charges", key, req, http.StatusCreated, &ch)
	return ch, err
}

// Get finds a charge by the idempotency key it was created with.
func (c *Client) Get(ctx context.Context, key string) (Charge, error) {
	var ch Charge
	err := c.c.Do(ctx, http.MethodGet, "/v1/charges/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &ch)
	return ch, err
}

// Refund returns amount of the paid charge to the payer, once per key: repeating the
// same request returns the same refund.
func (c *Client) Refund(ctx context.Context, key, chargeID string, req CreateRefund) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodPost, "/v1/charges/"+url.PathEscape(chargeID)+"/refunds", key, req, http.StatusCreated, &r)
	return r, err
}

// GetRefund finds a refund by the idempotency key it was created with.
func (c *Client) GetRefund(ctx context.Context, key string) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodGet, "/v1/refunds/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &r)
	return r, err
}

// Pay is the control endpoint that plays the payer: it pays the charge now.
func (c *Client) Pay(ctx context.Context, id string) (Charge, error) {
	var ch Charge
	err := c.c.Do(ctx, http.MethodPost, "/control/charges/"+url.PathEscape(id)+"/pay", "", nil, http.StatusOK, &ch)
	return ch, err
}

// Redeliver is the control endpoint that sends an event again, as a provider that
// retries a delivery it thinks was lost.
func (c *Client) Redeliver(ctx context.Context, eventID string) error {
	return c.c.Do(ctx, http.MethodPost, "/control/events/"+url.PathEscape(eventID)+"/deliver", "", nil, http.StatusNoContent, nil)
}

// InjectFault is the control endpoint that schedules failures.
func (c *Client) InjectFault(ctx context.Context, f Fault) error {
	return c.c.Do(ctx, http.MethodPost, "/control/faults", "", f, http.StatusNoContent, nil)
}

// problemError maps only the exact problem codes it knows; anything else, such as a
// 500 or a proxy's HTML page, stays ErrBadResponse.
func problemError(status int, code string) error {
	switch {
	case status == http.StatusUnprocessableEntity && code == CodeRejected:
		return ErrRejected
	case status == http.StatusUnprocessableEntity && code == CodeRefundRejected:
		return ErrRefundRejected
	case status == http.StatusNotFound && code == CodeRefundNotFound:
		return ErrRefundNotFound
	case status == http.StatusUnprocessableEntity && code == CodeKeyReused:
		return ErrKeyReused
	case status == http.StatusNotFound && code == CodeNotFound:
		return ErrNotFound
	case status == http.StatusConflict && code == CodeNotPayable:
		return ErrNotPayable
	case status == http.StatusNotFound && code == CodeEventNotFound:
		return ErrEventNotFound
	case status == http.StatusBadGateway && code == CodeDeliveryFailed:
		return ErrDeliveryFailed
	default:
		return nil
	}
}
