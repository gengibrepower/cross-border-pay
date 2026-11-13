package cardsim

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

var (
	ErrNotFound = errors.New("cardsim: authorization not found")
	// ErrKeyReused is the same idempotency key sent with another request.
	ErrKeyReused = errors.New("cardsim: idempotency key reused with another request")
	// ErrNotCapturable is a capture of an authorization that is declined, expired, or
	// already captured with another amount, or of more than it holds.
	ErrNotCapturable = errors.New("cardsim: authorization cannot be captured")
	// ErrNotVoidable is a void of an authorization already captured, declined or
	// expired.
	ErrNotVoidable = errors.New("cardsim: authorization cannot be voided")
	// ErrRefundRejected is the simulator refusing a refund for good; the refund is
	// stored and GetRefund finds it rejected.
	ErrRefundRejected = errors.New("cardsim: refund rejected")
	ErrRefundNotFound = errors.New("cardsim: refund not found")
	ErrBadResponse    = simkit.ErrBadResponse
)

type Client struct {
	c *simkit.Client
}

// NewClient talks to the simulator at baseURL. httpClient nil means one with
// simkit.DefaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	c, err := simkit.NewClient("cardsim", baseURL, httpClient, problemError)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Authorize creates the authorization once per key: a decline is an answer, returned
// with status declined, not an error.
func (c *Client) Authorize(ctx context.Context, key string, req Authorize) (Authorization, error) {
	var a Authorization
	err := c.c.Do(ctx, http.MethodPost, "/v1/authorizations", key, req, http.StatusCreated, &a)
	return a, err
}

// Get finds an authorization by the idempotency key it was created with.
func (c *Client) Get(ctx context.Context, key string) (Authorization, error) {
	var a Authorization
	err := c.c.Do(ctx, http.MethodGet, "/v1/authorizations/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &a)
	return a, err
}

// Capture charges amount of the authorization.
func (c *Client) Capture(ctx context.Context, id string, req Capture) (Authorization, error) {
	var a Authorization
	err := c.c.Do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(id)+"/capture", "", req, http.StatusOK, &a)
	return a, err
}

// Void releases an authorization not captured; voiding it again returns it as it is.
func (c *Client) Void(ctx context.Context, id string) (Authorization, error) {
	var a Authorization
	err := c.c.Do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(id)+"/void", "", nil, http.StatusOK, &a)
	return a, err
}

// Refund returns amount of the captured authorization, once per key.
func (c *Client) Refund(ctx context.Context, key, id string, req CreateRefund) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(id)+"/refunds", key, req, http.StatusCreated, &r)
	return r, err
}

// GetRefund finds a refund by the idempotency key it was created with.
func (c *Client) GetRefund(ctx context.Context, key string) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodGet, "/v1/refunds/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &r)
	return r, err
}

// InjectFault is the control endpoint that schedules failures.
func (c *Client) InjectFault(ctx context.Context, f Fault) error {
	return c.c.Do(ctx, http.MethodPost, "/control/faults", "", f, http.StatusNoContent, nil)
}

func problemError(status int, code string) error {
	switch {
	case status == http.StatusNotFound && code == CodeNotFound:
		return ErrNotFound
	case status == http.StatusUnprocessableEntity && code == CodeKeyReused:
		return ErrKeyReused
	case status == http.StatusConflict && code == CodeNotCapturable:
		return ErrNotCapturable
	case status == http.StatusConflict && code == CodeNotVoidable:
		return ErrNotVoidable
	case status == http.StatusUnprocessableEntity && code == CodeRefundRejected:
		return ErrRefundRejected
	case status == http.StatusNotFound && code == CodeRefundNotFound:
		return ErrRefundNotFound
	default:
		return nil
	}
}
