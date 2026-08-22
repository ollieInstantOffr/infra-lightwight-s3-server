// Package httpx resolves the request properties that a reverse proxy rewrites.
//
// This server always sits behind nginx proxy manager, which terminates TLS and
// forwards plain HTTP. The hostname and scheme the client actually used survive
// only in X-Forwarded-* headers — and those headers are trivially forged, so
// they are honoured only from addresses the operator has declared trustworthy.
//
// Getting this wrong is a security bug rather than a cosmetic one: SigV4 signs
// the Host header, so an attacker who could dictate the host this server
// believes it was reached on could get signatures validated against a hostname
// the client never used.
package httpx

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ProxyTrust decides whether a request's forwarding headers may be believed.
type ProxyTrust struct {
	networks []*net.IPNet
}

// NewProxyTrust parses the configured CIDR list.
func NewProxyTrust(cidrs []string) (*ProxyTrust, error) {
	t := &ProxyTrust{}
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// A bare address is accepted as a single-host network, since writing
		// "10.0.0.5" rather than "10.0.0.5/32" is an easy and harmless mistake.
		if !strings.Contains(raw, "/") {
			ip := net.ParseIP(raw)
			if ip == nil {
				return nil, fmt.Errorf("trusted proxy %q is neither an IP address nor a CIDR", raw)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			t.networks = append(t.networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", raw, err)
		}
		t.networks = append(t.networks, network)
	}
	return t, nil
}

// Trusted reports whether the immediate peer is a configured proxy.
func (t *ProxyTrust) Trusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range t.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// Host returns the hostname the client used, which is what SigV4 signed.
//
// X-Forwarded-Host may carry a comma-separated chain when several proxies are
// involved; the first entry is the original client-facing host.
func (t *ProxyTrust) Host(r *http.Request) string {
	if t.Trusted(r) {
		if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
			first, _, _ := strings.Cut(forwarded, ",")
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}
	return r.Host
}

// Scheme returns the scheme the client used. The console needs it to decide
// whether to mark session cookies Secure, and magic-link emails need it to
// build an absolute URL that works.
func (t *ProxyTrust) Scheme(r *http.Request) string {
	if t.Trusted(r) {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			first, _, _ := strings.Cut(proto, ",")
			if first = strings.ToLower(strings.TrimSpace(first)); first == "http" || first == "https" {
				return first
			}
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// ClientIP returns the originating client address, for rate limiting and logs.
func (t *ProxyTrust) ClientIP(r *http.Request) string {
	if t.Trusted(r) {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first, _, _ := strings.Cut(forwarded, ",")
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
