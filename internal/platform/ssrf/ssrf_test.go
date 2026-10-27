package ssrf_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/gengibrepower/cross-border-pay/internal/platform/ssrf"
)

var (
	strict = ssrf.Policy{}
	dev    = ssrf.Policy{AllowPrivate: true}
)

// Each blocked range, with the addresses on its edges and the first ones outside.
func TestAllowedIP(t *testing.T) {
	for _, tc := range []struct {
		ip          string
		strict, dev bool
	}{
		{"93.184.215.14", true, true},
		{"2606:2800:21f:cb07:6820:80da:af6b:8b2c", true, true},
		{"127.0.0.1", false, true},
		{"127.255.255.255", false, true},
		{"::1", false, true},
		{"10.0.0.0", false, true},
		{"10.255.255.255", false, true},
		{"11.0.0.0", true, true},
		{"172.15.255.255", true, true},
		{"172.16.0.0", false, true},
		{"172.31.255.255", false, true},
		{"172.32.0.0", true, true},
		{"192.168.0.1", false, true},
		{"192.169.0.1", true, true},
		{"fc00::1", false, true},
		{"fd00:ec2::254", false, true},
		{"169.254.169.254", false, false},
		{"169.254.0.0", false, false},
		{"169.255.0.0", true, true},
		{"fe80::1", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"::ffff:10.0.0.1", false, true},
		{"::ffff:93.184.215.14", true, true},
		{"100.64.0.0", false, true},
		{"100.127.255.255", false, true},
		{"100.128.0.0", true, true},
		{"0.0.0.0", false, false},
		{"0.1.2.3", false, true},
		{"::", false, false},
		{"224.0.0.1", false, false},
		{"ff02::1", false, false},
		{"240.0.0.1", false, true},
		{"255.255.255.255", false, true},
		{"198.18.0.1", false, true},
		{"64:ff9b::a00:1", false, true},
	} {
		ip := netip.MustParseAddr(tc.ip)
		if got := strict.AllowedIP(ip); got != tc.strict {
			t.Errorf("strict AllowedIP(%s) = %v, want %v", ip, got, tc.strict)
		}
		if got := dev.AllowedIP(ip); got != tc.dev {
			t.Errorf("dev AllowedIP(%s) = %v, want %v", ip, got, tc.dev)
		}
	}
	if strict.AllowedIP(netip.Addr{}) || dev.AllowedIP(netip.Addr{}) {
		t.Error("the zero address is allowed")
	}
}

func TestValidateURL(t *testing.T) {
	atLimit := "https://example.com/" + strings.Repeat("a", ssrf.MaxURLLength-len("https://example.com/"))
	long := atLimit + "a"
	for _, tc := range []struct {
		url         string
		strict, dev error
	}{
		{"https://gamestore.example/webhooks", nil, nil},
		{"https://gamestore.example:8443/hooks?src=cbp", nil, nil},
		{"https://93.184.215.14/hooks", nil, nil},
		{"http://gamestore.example/hooks", ssrf.ErrInvalidURL, nil},
		{"ftp://gamestore.example/hooks", ssrf.ErrInvalidURL, ssrf.ErrInvalidURL},
		{"https://user:pass@gamestore.example/", ssrf.ErrInvalidURL, ssrf.ErrInvalidURL},
		{"https:///hooks", ssrf.ErrInvalidURL, ssrf.ErrInvalidURL},
		{"https:gamestore.example", ssrf.ErrInvalidURL, ssrf.ErrInvalidURL},
		{"https://gamestore.example/#top", ssrf.ErrInvalidURL, ssrf.ErrInvalidURL},
		{"gamestore.example/hooks", ssrf.ErrInvalidURL, ssrf.ErrInvalidURL},
		{"", ssrf.ErrInvalidURL, ssrf.ErrInvalidURL},
		{atLimit, nil, nil},
		{long, ssrf.ErrInvalidURL, ssrf.ErrInvalidURL},
		{"https://127.0.0.1/", ssrf.ErrForbiddenURL, nil},
		{"https://[::1]:8443/", ssrf.ErrForbiddenURL, nil},
		{"https://10.1.2.3/", ssrf.ErrForbiddenURL, nil},
		{"https://169.254.169.254/latest/meta-data/", ssrf.ErrForbiddenURL, ssrf.ErrForbiddenURL},
		{"https://[::ffff:169.254.169.254]/", ssrf.ErrForbiddenURL, ssrf.ErrForbiddenURL},
		{"https://localhost/", ssrf.ErrForbiddenURL, nil},
		{"https://LOCALHOST./", ssrf.ErrForbiddenURL, nil},
		{"https://api.localhost/", ssrf.ErrForbiddenURL, nil},
		{"https://metadata.google.internal/", ssrf.ErrForbiddenURL, nil},
		{"https://printer.local/", ssrf.ErrForbiddenURL, nil},
		{"https://2130706433/", ssrf.ErrForbiddenURL, ssrf.ErrForbiddenURL},
		{"https://0x7f.1/", ssrf.ErrForbiddenURL, ssrf.ErrForbiddenURL},
		{"https://127.1/", ssrf.ErrForbiddenURL, ssrf.ErrForbiddenURL},
		{"https://localhost.example/", nil, nil},
		{"https://internal.example/", nil, nil},
		{"https://a1.be/", nil, nil},
		{"https://cafe.de/", nil, nil},
		{"https://2024.events/", nil, nil},
	} {
		for _, p := range []struct {
			name   string
			policy ssrf.Policy
			want   error
		}{{"strict", strict, tc.strict}, {"dev", dev, tc.dev}} {
			_, err := p.policy.ValidateURL(tc.url)
			if !errors.Is(err, p.want) {
				t.Errorf("%s ValidateURL(%.60q) = %v, want %v", p.name, tc.url, err, p.want)
			}
		}
	}
}
