package adapters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
	"github.com/gengibrepower/cross-border-pay/internal/webhooks/domain"
)

var ErrForbiddenAddress = errors.New("webhooks: address not allowed")

// maxDrain is how much of an answer is read so its connection can be reused; the body
// itself means nothing, only the status does.
const maxDrain = 64 << 10

// HTTPSender posts webhooks. The URL was checked when the merchant registered it, but
// its name may resolve somewhere else now (DNS rebinding), so every IP is checked again
// when connecting, after DNS, with the same policy.
type HTTPSender struct {
	client *http.Client
	policy ssrf.Policy
}

func NewHTTPSender(policy ssrf.Policy, timeout time.Duration) *HTTPSender {
	dialer := &net.Dialer{Timeout: timeout, Control: checkAddress(policy)}
	return &HTTPSender{
		policy: policy,
		client: &http.Client{
			// No Proxy: through a proxy, the dialer would check the proxy's IP, not the
			// merchant's.
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				ForceAttemptHTTP2:     true,
				TLSHandshakeTimeout:   timeout,
				ResponseHeaderTimeout: timeout,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       90 * time.Second,
			},
			Timeout: timeout,
			// A redirect is answered as is and counts as a failure: following it would
			// let a public URL send the post inside the network.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// checkAddress runs for every address the dialer tries, after DNS and before connect.
func checkAddress(policy ssrf.Policy) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrForbiddenAddress, address, err)
		}
		if !policy.AllowedIP(ap.Addr()) {
			return fmt.Errorf("%w: %s", ErrForbiddenAddress, ap.Addr())
		}
		return nil
	}
}

func (s *HTTPSender) Send(ctx context.Context, rawURL, signature string, body []byte) (int, error) {
	u, err := s.policy.ValidateURL(rawURL)
	if err != nil {
		return 0, fmt.Errorf("webhooks: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("webhooks: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cross-border-pay-webhooks/1")
	req.Header.Set(domain.SignatureHeader, signature)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("webhooks: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	return resp.StatusCode, nil
}
