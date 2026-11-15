package cardsim

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/gengibrepower/cross-border-pay/internal/providers/simkit"
)

var (
	ErrNotFound      = errors.New("cardsim: authorization not found")
	ErrKeyReused     = errors.New("cardsim: idempotency key reused with another request")
	ErrNotCapturable = errors.New("cardsim: authorization cannot be captured")
	ErrNotVoidable   = errors.New("cardsim: authorization cannot be voided")
	// ErrRefundRejected is final: GetRefund finds the refund rejected.
	ErrRefundRejected     = errors.New("cardsim: refund rejected")
	ErrRefundNotFound     = errors.New("cardsim: refund not found")
	ErrNotDisputable      = errors.New("cardsim: authorization cannot be charged back")
	ErrChargebackNotFound = errors.New("cardsim: chargeback not found")
	ErrChargebackClosed   = errors.New("cardsim: chargeback can no longer take this answer")
	ErrEventNotFound      = errors.New("cardsim: event not found")
	ErrDeliveryFailed     = errors.New("cardsim: event delivery failed")
	ErrBadResponse        = simkit.ErrBadResponse
)

type Client struct {
	c *simkit.Client
}

// NewClient's nil httpClient uses simkit.DefaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	c, err := simkit.NewClient("cardsim", baseURL, httpClient, problemError)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Authorize returns a decline as a status, not an error.
func (c *Client) Authorize(ctx context.Context, key string, req Authorize) (Authorization, error) {
	var a Authorization
	err := c.c.Do(ctx, http.MethodPost, "/v1/authorizations", key, req, http.StatusCreated, &a)
	return a, err
}

func (c *Client) Get(ctx context.Context, key string) (Authorization, error) {
	var a Authorization
	err := c.c.Do(ctx, http.MethodGet, "/v1/authorizations/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &a)
	return a, err
}

func (c *Client) Capture(ctx context.Context, id string, req Capture) (Authorization, error) {
	var a Authorization
	err := c.c.Do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(id)+"/capture", "", req, http.StatusOK, &a)
	return a, err
}

// Void again returns the authorization as it is.
func (c *Client) Void(ctx context.Context, id string) (Authorization, error) {
	var a Authorization
	err := c.c.Do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(id)+"/void", "", nil, http.StatusOK, &a)
	return a, err
}

func (c *Client) Refund(ctx context.Context, key, id string, req CreateRefund) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodPost, "/v1/authorizations/"+url.PathEscape(id)+"/refunds", key, req, http.StatusCreated, &r)
	return r, err
}

func (c *Client) GetRefund(ctx context.Context, key string) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodGet, "/v1/refunds/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &r)
	return r, err
}

func (c *Client) GetChargeback(ctx context.Context, id string) (Chargeback, error) {
	var cb Chargeback
	err := c.c.Do(ctx, http.MethodGet, "/v1/chargebacks/"+url.PathEscape(id), "", nil, http.StatusOK, &cb)
	return cb, err
}

// Accept loses the chargeback at once. Accepting again returns it as it is.
func (c *Client) Accept(ctx context.Context, id string) (Chargeback, error) {
	var cb Chargeback
	err := c.c.Do(ctx, http.MethodPost, "/v1/chargebacks/"+url.PathEscape(id)+"/accept", "", nil, http.StatusOK, &cb)
	return cb, err
}

// Contest again returns the chargeback as it is.
func (c *Client) Contest(ctx context.Context, id string, req Contest) (Chargeback, error) {
	var cb Chargeback
	err := c.c.Do(ctx, http.MethodPost, "/v1/chargebacks/"+url.PathEscape(id)+"/contest", "", req, http.StatusOK, &cb)
	return cb, err
}

func (c *Client) OpenChargeback(ctx context.Context, authorization string, req OpenChargeback) (Chargeback, error) {
	var cb Chargeback
	err := c.c.Do(ctx, http.MethodPost, "/control/authorizations/"+url.PathEscape(authorization)+"/chargebacks", "", req, http.StatusCreated, &cb)
	return cb, err
}

// ResolveChargeback's Lost also closes a chargeback nobody answered.
func (c *Client) ResolveChargeback(ctx context.Context, id string, outcome ChargebackStatus) (Chargeback, error) {
	var cb Chargeback
	err := c.c.Do(ctx, http.MethodPost, "/control/chargebacks/"+url.PathEscape(id)+"/resolve", "", ResolveChargeback{Outcome: outcome}, http.StatusOK, &cb)
	return cb, err
}

func (c *Client) Redeliver(ctx context.Context, id string) error {
	return c.c.Do(ctx, http.MethodPost, "/control/events/"+url.PathEscape(id)+"/deliver", "", nil, http.StatusNoContent, nil)
}

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
	case status == http.StatusConflict && code == CodeNotDisputable:
		return ErrNotDisputable
	case status == http.StatusNotFound && code == CodeChargebackNotFound:
		return ErrChargebackNotFound
	case status == http.StatusConflict && code == CodeChargebackClosed:
		return ErrChargebackClosed
	case status == http.StatusNotFound && code == CodeEventNotFound:
		return ErrEventNotFound
	case status == http.StatusBadGateway && code == CodeDeliveryFailed:
		return ErrDeliveryFailed
	default:
		return nil
	}
}
