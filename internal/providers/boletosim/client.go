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
	ErrRejected  = errors.New("boletosim: boleto rejected")
	ErrNotFound  = errors.New("boletosim: boleto not found")
	ErrKeyReused = errors.New("boletosim: idempotency key reused with another request")
	// ErrOverdue is decision 53.
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

// NewClient's nil httpClient uses simkit.DefaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	c, err := simkit.NewClient("boletosim", baseURL, httpClient, problemError)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// Create is once per key, so a create whose answer was lost can be retried or looked
// up with Get.
func (c *Client) Create(ctx context.Context, key string, req *CreateBoleto) (Boleto, error) {
	var b Boleto
	err := c.c.Do(ctx, http.MethodPost, "/v1/boletos", key, req, http.StatusCreated, &b)
	return b, err
}

func (c *Client) Get(ctx context.Context, key string) (Boleto, error) {
	var b Boleto
	err := c.c.Do(ctx, http.MethodGet, "/v1/boletos/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &b)
	return b, err
}

func (c *Client) Refund(ctx context.Context, key, boletoID string, req CreateRefund) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodPost, "/v1/boletos/"+url.PathEscape(boletoID)+"/refunds", key, req, http.StatusCreated, &r)
	return r, err
}

func (c *Client) GetRefund(ctx context.Context, key string) (Refund, error) {
	var r Refund
	err := c.c.Do(ctx, http.MethodGet, "/v1/refunds/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &r)
	return r, err
}

func (c *Client) Pay(ctx context.Context, id string) (Boleto, error) {
	var b Boleto
	err := c.c.Do(ctx, http.MethodPost, "/control/boletos/"+url.PathEscape(id)+"/pay", "", nil, http.StatusOK, &b)
	return b, err
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
