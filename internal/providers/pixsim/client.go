package pixsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
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
	ErrBadResponse    = errors.New("pixsim: unexpected response")
	// ErrRefundRejected is the simulator refusing a refund for good; like ErrRejected,
	// the refund is stored and Get finds it rejected.
	ErrRefundRejected = errors.New("pixsim: refund rejected")
	ErrRefundNotFound = errors.New("pixsim: refund not found")
)

// DefaultTimeout bounds every call; a call that hits it is ambiguous, not failed.
const DefaultTimeout = 10 * time.Second

const maxResponse = 1 << 20

type Client struct {
	base *url.URL
	http *http.Client
}

// NewClient talks to the simulator at baseURL. httpClient nil means one with
// DefaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("pixsim: base URL %q must be an absolute http(s) URL", baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{base: base, http: httpClient}, nil
}

// Create creates the charge once per key: repeating the same request returns the same
// charge, so a create whose answer was lost can be retried or looked up with Get.
func (c *Client) Create(ctx context.Context, key string, req CreateCharge) (Charge, error) {
	var ch Charge
	err := c.do(ctx, http.MethodPost, "/v1/charges", key, req, http.StatusCreated, &ch)
	return ch, err
}

// Get finds a charge by the idempotency key it was created with.
func (c *Client) Get(ctx context.Context, key string) (Charge, error) {
	var ch Charge
	err := c.do(ctx, http.MethodGet, "/v1/charges/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &ch)
	return ch, err
}

// Refund returns amount of the paid charge to the payer, once per key: repeating the
// same request returns the same refund.
func (c *Client) Refund(ctx context.Context, key, chargeID string, req CreateRefund) (Refund, error) {
	var r Refund
	err := c.do(ctx, http.MethodPost, "/v1/charges/"+url.PathEscape(chargeID)+"/refunds", key, req, http.StatusCreated, &r)
	return r, err
}

// GetRefund finds a refund by the idempotency key it was created with.
func (c *Client) GetRefund(ctx context.Context, key string) (Refund, error) {
	var r Refund
	err := c.do(ctx, http.MethodGet, "/v1/refunds/by-key/"+url.PathEscape(key), "", nil, http.StatusOK, &r)
	return r, err
}

// Pay is the control endpoint that plays the payer: it pays the charge now.
func (c *Client) Pay(ctx context.Context, id string) (Charge, error) {
	var ch Charge
	err := c.do(ctx, http.MethodPost, "/control/charges/"+url.PathEscape(id)+"/pay", "", nil, http.StatusOK, &ch)
	return ch, err
}

// Redeliver is the control endpoint that sends an event again, as a provider that
// retries a delivery it thinks was lost.
func (c *Client) Redeliver(ctx context.Context, eventID string) error {
	return c.do(ctx, http.MethodPost, "/control/events/"+url.PathEscape(eventID)+"/deliver", "", nil, http.StatusNoContent, nil)
}

// InjectFault is the control endpoint that schedules failures.
func (c *Client) InjectFault(ctx context.Context, f Fault) error {
	return c.do(ctx, http.MethodPost, "/control/faults", "", f, http.StatusNoContent, nil)
}

func (c *Client) do(ctx context.Context, method, path, key string, body any, want int, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("pixsim: encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath(path).String(), reader)
	if err != nil {
		return fmt.Errorf("pixsim: request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pixsim: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("pixsim: %s %s: read response: %w", method, path, err)
	}

	if resp.StatusCode != want {
		return fmt.Errorf("pixsim: %s %s: %w", method, path, problemError(resp.StatusCode, data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("pixsim: %s %s: %w: %w", method, path, ErrBadResponse, err)
	}
	return nil
}

// problemError maps only the exact problem codes it knows; anything else, such as a
// 500 or a proxy's HTML page, stays ErrBadResponse.
func problemError(status int, body []byte) error {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &p)
	switch {
	case status == http.StatusUnprocessableEntity && p.Code == CodeRejected:
		return ErrRejected
	case status == http.StatusUnprocessableEntity && p.Code == CodeRefundRejected:
		return ErrRefundRejected
	case status == http.StatusNotFound && p.Code == CodeRefundNotFound:
		return ErrRefundNotFound
	case status == http.StatusUnprocessableEntity && p.Code == CodeKeyReused:
		return ErrKeyReused
	case status == http.StatusNotFound && p.Code == CodeNotFound:
		return ErrNotFound
	case status == http.StatusConflict && p.Code == CodeNotPayable:
		return ErrNotPayable
	case status == http.StatusNotFound && p.Code == CodeEventNotFound:
		return ErrEventNotFound
	case status == http.StatusBadGateway && p.Code == CodeDeliveryFailed:
		return ErrDeliveryFailed
	default:
		return fmt.Errorf("%w: status %d, code %q", ErrBadResponse, status, p.Code)
	}
}
