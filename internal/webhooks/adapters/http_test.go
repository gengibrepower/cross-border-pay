package adapters

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
	"github.com/gengibrepower/cross-border-pay/internal/webhooks/domain"
)

var dev = ssrf.Policy{AllowPrivate: true}

func TestSendPostsTheBodyWithItsSignature(t *testing.T) {
	var got struct {
		method, contentType, signature string
		body                           []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.contentType, got.signature = r.Method, r.Header.Get("Content-Type"), r.Header.Get(domain.SignatureHeader)
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	code, err := NewHTTPSender(dev, time.Second).Send(t.Context(), srv.URL+"/hooks", "t=1,v1=ab", []byte(`{"id":"evt_1"}`))
	if err != nil || code != http.StatusAccepted {
		t.Fatalf("Send() = %d, %v, want 202", code, err)
	}
	if got.method != http.MethodPost || got.contentType != "application/json" || got.signature != "t=1,v1=ab" || string(got.body) != `{"id":"evt_1"}` {
		t.Errorf("received %+v", got)
	}
}

// A redirect is answered as it came: following it would let a public URL send the
// post inside the network.
func TestSendDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/hooks", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/inside", http.StatusFound) })
	mux.HandleFunc("/inside", func(http.ResponseWriter, *http.Request) { followed.Add(1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code, err := NewHTTPSender(dev, time.Second).Send(t.Context(), srv.URL+"/hooks", "", []byte("{}"))
	if err != nil || code != http.StatusFound {
		t.Fatalf("Send() = %d, %v, want 302", code, err)
	}
	if followed.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestSendGivesUpAtTheTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	code, err := NewHTTPSender(dev, 100*time.Millisecond).Send(t.Context(), srv.URL, "", []byte("{}"))
	if err == nil || code != 0 {
		t.Fatalf("Send() = %d, %v, want no answer", code, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Send() took %v with a 100 ms timeout", d)
	}
}

func TestSendRefusesAURLThePolicyRefuses(t *testing.T) {
	_, err := NewHTTPSender(ssrf.Policy{}, time.Second).Send(t.Context(), "http://example.com/hooks", "", []byte("{}"))
	if !errors.Is(err, ssrf.ErrInvalidURL) {
		t.Errorf("Send() = %v, want %v", err, ssrf.ErrInvalidURL)
	}
}

// The registered URL can name a host that later resolves inside the network. The
// dialer checks the IP it is about to connect to, whatever the URL said: here the
// strict policy refuses the loopback the test server listens on.
func TestDialRefusesAnInternalAddressAfterDNS(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	resp, err := NewHTTPSender(ssrf.Policy{}, time.Second).client.Post(srv.URL, "application/json", nil) //nolint:noctx // the test's own request, no context to carry.
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrForbiddenAddress) {
		t.Errorf("Post() = %v, want %v", err, ErrForbiddenAddress)
	}
	if hits.Load() != 0 {
		t.Error("the server was reached")
	}
}

func TestCheckAddress(t *testing.T) {
	tests := []struct {
		address string
		policy  ssrf.Policy
		allowed bool
	}{
		{"93.184.215.14:443", ssrf.Policy{}, true},
		{"[2606:2800:21f:cb07:6820:80da:af6b:8b2c]:443", ssrf.Policy{}, true},
		{"127.0.0.1:443", ssrf.Policy{}, false},
		{"10.0.0.7:443", ssrf.Policy{}, false},
		{"[::1]:443", ssrf.Policy{}, false},
		{"127.0.0.1:443", dev, true},
		{"169.254.169.254:80", ssrf.Policy{}, false},
		{"169.254.169.254:80", dev, false},
		{"[::ffff:169.254.169.254]:80", dev, false},
		{"not-an-address", dev, false},
	}
	for _, tc := range tests {
		err := checkAddress(tc.policy)("tcp", tc.address, nil)
		if allowed := err == nil; allowed != tc.allowed {
			t.Errorf("checkAddress(%+v)(%s) = %v, want allowed %v", tc.policy, tc.address, err, tc.allowed)
		}
		if err != nil && !errors.Is(err, ErrForbiddenAddress) {
			t.Errorf("checkAddress(%s) = %v, want %v", tc.address, err, ErrForbiddenAddress)
		}
	}
}
