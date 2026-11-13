package simkit

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

// ErrBadResponse is an answer the client does not understand: a status it did not
// expect, a problem code it does not know, or a body it cannot read.
var ErrBadResponse = errors.New("simulator: unexpected response")

// DefaultTimeout bounds every call; a call that hits it is ambiguous, not failed.
const DefaultTimeout = 10 * time.Second

const maxResponse = 1 << 20

// Problems maps an error answer to the simulator's own error. It returns nil for one
// it does not know, which the client reports as ErrBadResponse.
type Problems func(status int, code string) error

// Client is the HTTP core of a simulator's client.
type Client struct {
	name     string
	base     *url.URL
	http     *http.Client
	problems Problems
}

// NewClient talks to the simulator named name at baseURL. httpClient nil means one
// with DefaultTimeout.
func NewClient(name, baseURL string, httpClient *http.Client, problems Problems) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("%s: base URL %q must be an absolute http(s) URL", name, baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{name: name, base: base, http: httpClient, problems: problems}, nil
}

// Do sends body as JSON, with key as the Idempotency-Key when it is not empty, and
// decodes the answer into out when its status is want.
func (c *Client) Do(ctx context.Context, method, path, key string, body any, want int, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: encode request: %w", c.name, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath(path).String(), reader)
	if err != nil {
		return fmt.Errorf("%s: request: %w", c.name, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %s %s: %w", c.name, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("%s: %s %s: read response: %w", c.name, method, path, err)
	}

	if resp.StatusCode != want {
		return fmt.Errorf("%s: %s %s: %w", c.name, method, path, c.problem(resp.StatusCode, data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %s %s: %w: %w", c.name, method, path, ErrBadResponse, err)
	}
	return nil
}

// problem maps only the exact problem codes the simulator knows; anything else, such
// as a 500 or a proxy's HTML page, stays ErrBadResponse.
func (c *Client) problem(status int, body []byte) error {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &p)
	if err := c.problems(status, p.Code); err != nil {
		return err
	}
	return fmt.Errorf("%w: status %d, code %q", ErrBadResponse, status, p.Code)
}
