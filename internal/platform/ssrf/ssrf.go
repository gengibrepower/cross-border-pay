// Package ssrf keeps webhooks from reaching inside our own network (section 4.5). The
// URL is checked when a merchant registers it, and every IP again after DNS, at the
// moment of connecting: a name can resolve somewhere else later (DNS rebinding).
package ssrf

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

var (
	ErrInvalidURL   = errors.New("ssrf: invalid webhook URL")
	ErrForbiddenURL = errors.New("ssrf: webhook URL points inside the network")
)

// MaxURLLength matches the column that stores it.
const MaxURLLength = 2048

// Policy says what may be reached. AllowPrivate is for development and the GameStore
// walkthrough, whose fake merchant lives on a private network and speaks http; it never
// opens cloud metadata.
type Policy struct {
	AllowPrivate bool
}

var (
	// Link-local holds the cloud metadata service at 169.254.169.254: never reachable.
	metadata = []netip.Prefix{
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fe80::/10"),
	}
	// Ranges that are not the public internet, beyond what netip names: this host
	// ("0.0.0.0/8"), carrier-grade NAT, IETF protocol assignments, benchmarking,
	// documentation and the reserved 240/4, which includes broadcast.
	internal = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	internalNames = []string{"localhost", ".localhost", ".internal", ".local"}
)

// AllowedIP reports whether a webhook may connect to ip.
func (p Policy) AllowedIP(ip netip.Addr) bool {
	// ::ffff:10.0.0.1 is 10.0.0.1 to the kernel.
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, r := range metadata {
		if r.Contains(ip) {
			return false
		}
	}
	if p.AllowPrivate {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return false
	}
	for _, r := range internal {
		if r.Contains(ip) {
			return false
		}
	}
	return true
}

// ValidateURL checks a webhook URL without resolving it: syntax, scheme, no
// credentials, and a host that is not an internal name or IP. The IPs a name resolves
// to are checked when connecting.
func (p Policy) ValidateURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > MaxURLLength {
		return nil, fmt.Errorf("%w: must have 1 to %d characters", ErrInvalidURL, MaxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	switch {
	case u.Scheme != "https" && (!p.AllowPrivate || u.Scheme != "http"):
		return nil, fmt.Errorf("%w: scheme must be https, got %q", ErrInvalidURL, u.Scheme)
	case u.User != nil:
		return nil, fmt.Errorf("%w: must not carry credentials", ErrInvalidURL)
	case u.Hostname() == "" || u.Opaque != "":
		return nil, fmt.Errorf("%w: has no host", ErrInvalidURL)
	case u.Fragment != "":
		return nil, fmt.Errorf("%w: must not have a fragment", ErrInvalidURL)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		if !p.AllowedIP(ip) {
			return nil, fmt.Errorf("%w: %s", ErrForbiddenURL, ip)
		}
		return u, nil
	}
	// 2130706433, 0x7f.1 and 127.1 are 127.0.0.1 to some resolvers and a name to others.
	if numericHost(host) {
		return nil, fmt.Errorf("%w: %s is a numeric host that is not a plain IP", ErrForbiddenURL, host)
	}
	if !p.AllowPrivate && internalName(host) {
		return nil, fmt.Errorf("%w: %s", ErrForbiddenURL, host)
	}
	return u, nil
}

func internalName(host string) bool {
	for _, n := range internalNames {
		if host == strings.TrimPrefix(n, ".") || (strings.HasPrefix(n, ".") && strings.HasSuffix(host, n)) {
			return true
		}
	}
	return false
}

// numericHost reports a host whose every label is a number, in decimal or 0x hex, as
// inet_aton reads them.
func numericHost(host string) bool {
	for label := range strings.SplitSeq(host, ".") {
		digits, hex := strings.CutPrefix(label, "0x")
		set := "0123456789"
		if hex {
			set = "0123456789abcdef"
		}
		if (digits == "" && !hex) || strings.Trim(digits, set) != "" {
			return false
		}
	}
	return true
}
