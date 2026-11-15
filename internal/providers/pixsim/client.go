package pixsim

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/gengibrepower/cross-border-pay/internal/calendar"

	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

var (
	// ErrRejected is the simulator refusing a charge for good. It is the only error
	// that says the provider did nothing; every other one is ambiguous.
	ErrRejected       = errors.New("pixsim: charge rejected")
	ErrNotFound       = errors.New("pixsim: charge not found")
	ErrKeyReused      = errors.New("pixsim: idempotency key reused with another charge")
	ErrNotPayable     = errors.New("pixsim: charge cannot be paid")
	ErrEventNotFound  = errors.New("pixsim: event not found")
	ErrDeliveryFailed = errors.New("pixsim: event delivery failed")
	ErrBadResponse    = simkit.ErrBadResponse
	// ErrRefundRejected is final: GetRefund finds the refund rejected.
	ErrRefundRejected = errors.New("pixsim: refund rejected")
	ErrRefundNotFound = errors.New("pixsim: refund not found")
)

// DefaultTimeout bounds every call; a call that hits it is ambiguous, not failed.
const DefaultTimeout = simkit.DefaultTimeout

type Client struct {
	c *simkit.Client
}

// NewClient's nil httpClient uses DefaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	c, err := simkit.NewClient("pixsim", baseURL, httpClient, problemError)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Create is once per key, so a create whose answer was lost can be retried or looked
// up with Get.
func (c *Client) Create(ctx context.Context, key string, req CreateCharge) (Charge, error) {
	var ch Charge
	err := c.c.Do(ctx, http.MethodPost, "/v1/charges", key, req, http.StatusCreated, &ch)
	return ch, err
}

func (c *Client) Get(ctx context.Context, key string) (Charge, error) {
	var ch Charge
	err := c.c.Do(ctx, http.MethodGet, "/v1/charges/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &ch)
	return ch, err
}

func (c *Client) Refund(ctx context.Context, key, chargeID string, req CreateRefund) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodPost, "/v1/charges/"+url.PathEscape(chargeID)+"/refunds", key, req, http.StatusCreated, &r)
	return r, err
}

func (c *Client) GetRefund(ctx context.Context, key string) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodGet, "/v1/refunds/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &r)
	return r, err
}

func (c *Client) Pay(ctx context.Context, id string) (Charge, error) {
	var ch Charge
	err := c.c.Do(ctx, http.MethodPost, "/control/charges/"+url.PathEscape(id)+"/pay", "", nil, http.StatusOK, &ch)
	return ch, err
}

func (c *Client) Redeliver(ctx context.Context, eventID string) error {
	return c.c.Do(ctx, http.MethodPost, "/control/events/"+url.PathEscape(eventID)+"/deliver", "", nil, http.StatusNoContent, nil)
}

func (c *Client) InjectFault(ctx context.Context, f Fault) error {
	return c.c.Do(ctx, http.MethodPost, "/control/faults", "", f, http.StatusNoContent, nil)
}

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

func (c *Client) Statement(ctx context.Context, date calendar.Date) (Statement, error) {
	return c.c.Statement(ctx, date)
}
