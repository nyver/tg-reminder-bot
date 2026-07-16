// Package netsafe provides an SSRF-hardened HTTP client for providers that
// fetch arbitrary user-supplied URLs (e.g. an RSS feed link).
package netsafe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// lookupIPAddr is overridden in tests to avoid depending on real DNS/network.
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// SafeClient returns an *http.Client suitable for fetching arbitrary
// user-supplied URLs.
//
// With proxyURL empty, its DialContext resolves the host and rejects
// private/loopback/link-local addresses at connect time, closing the
// DNS-rebinding window that exists when validation and dialing are separate
// steps.
//
// With proxyURL set (http, https, socks5 or socks5h), requests are routed
// through that proxy instead — e.g. to reach a feed whose host blocks this
// server's own IP range. The proxy, not this process, resolves and connects
// to the destination, so the direct-dial SSRF guard above does not apply to
// proxied requests; this mirrors provider/price's existing headless+proxy
// mode, which has the same trust boundary (the operator who configures
// proxy_url is trusted not to point it at an SSRF pivot).
func SafeClient(timeout time.Duration, proxyURL string) (*http.Client, error) {
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy_url: %w", err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("unsupported proxy_url scheme %q (want http, https, socks5 or socks5h)", u.Scheme)
		}
		if u.Hostname() == "" {
			return nil, fmt.Errorf("invalid proxy_url: host is required")
		}
		return &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:           http.ProxyURL(u),
				IdleConnTimeout: 30 * time.Second,
			},
		}, nil
	}

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if isPrivateHost(host) {
			return nil, fmt.Errorf("connection to private host %q blocked", host)
		}
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, err := lookupIPAddr(lookupCtx, host)
		cancel()
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no addresses resolved for %s", host)
		}
		for _, a := range addrs {
			if isPrivateIP(a.IP) {
				return nil, fmt.Errorf("connection to private IP %s (->%s) blocked", host, a.IP)
			}
		}
		// Connect to a specific resolved IP (never back through the hostname)
		// so the OS cannot silently re-resolve it to a different (private)
		// address mid-flight. Try every resolved address in order rather than
		// only the first: hosts commonly resolve to multiple addresses (e.g.
		// an AAAA record with no IPv6 egress from the container, or one dead
		// IP in a round-robin pool), and hard-coding the first would hang the
		// whole request until the outer client timeout even though another
		// resolved address is perfectly reachable.
		d := net.Dialer{}
		return dialFirstReachable(ctx, network, addrs, port, d.DialContext)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:     dial,
			IdleConnTimeout: 30 * time.Second,
		},
	}, nil
}

// dialFirstReachable tries every address in addrs, in order, returning the
// first successful connection. All addresses have already passed the
// private-IP check by the time this runs; this only handles the case where a
// resolved address is otherwise unreachable (dead, wrong address family,
// etc.). The dial parameter is injected so tests can exercise the fallback
// without opening real sockets.
func dialFirstReachable(ctx context.Context, network string, addrs []net.IPAddr, port string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (net.Conn, error) {
	var lastErr error
	for _, a := range addrs {
		conn, err := dial(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// ValidateURL rejects unsupported schemes and obviously-private hosts before
// any request is made. SafeClient's DialContext is the authoritative guard
// against DNS rebinding; this is a cheap early check that fails fast with a
// clearer error.
func ValidateURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("host is required")
	}
	if isPrivateHost(host) {
		return fmt.Errorf("private host not allowed: %s", host)
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := lookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("resolve host %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve host %s: no addresses", host)
	}
	for _, addr := range addrs {
		if isPrivateIP(addr.IP) {
			return fmt.Errorf("private resolved address not allowed: %s -> %s", host, addr.IP)
		}
	}
	return nil
}

func isPrivateHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "0.0.0.0" {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return isPrivateIP(ip)
}

// privateNets contains non-public address ranges that user-supplied URLs must
// never reach, including carrier-grade NAT and reserved/multicast networks.
var privateNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"0.0.0.0/8",      // unspecified/current network
		"127.0.0.0/8",    // loopback
		"10.0.0.0/8",     // RFC-1918 class A
		"100.64.0.0/10",  // carrier-grade NAT
		"172.16.0.0/12",  // RFC-1918 class B (172.16-31)
		"192.168.0.0/16", // RFC-1918 class C
		"169.254.0.0/16", // link-local / cloud metadata (169.254.169.254)
		"198.18.0.0/15",  // benchmark networks
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved
		"::/128",         // IPv6 unspecified
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA
		"fe80::/10",      // IPv6 link-local
		"ff00::/8",       // IPv6 multicast
	} {
		_, network, _ := net.ParseCIDR(cidr)
		if network != nil {
			privateNets = append(privateNets, network)
		}
	}
}

func isPrivateIP(ip net.IP) bool {
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
