package netsafe

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// fakeLookup replaces DNS resolution in tests so ValidateURL is deterministic
// and doesn't depend on real network access.
func fakeLookup(t *testing.T, results map[string][]net.IPAddr) {
	t.Helper()
	orig := lookupIPAddr
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if addrs, ok := results[host]; ok {
			return addrs, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	t.Cleanup(func() { lookupIPAddr = orig })
}

func TestValidateURL(t *testing.T) {
	ctx := context.Background()
	fakeLookup(t, map[string][]net.IPAddr{
		"example.com": {{IP: net.ParseIP("93.184.216.34")}},
	})

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"public https", "https://example.com/feed.xml", false},
		{"public http", "http://example.com/feed.xml", false},
		{"ftp scheme rejected", "ftp://example.com/feed.xml", true},
		{"localhost rejected", "http://localhost/feed.xml", true},
		{"loopback IP rejected", "http://127.0.0.1/feed.xml", true},
		{"link-local metadata rejected", "http://169.254.169.254/feed.xml", true},
		{"private class A rejected", "http://10.0.0.1/feed.xml", true},
		{"private class C rejected", "http://192.168.1.1/feed.xml", true},
		{"empty host rejected", "http:///feed.xml", true},
		{"malformed url rejected", "://not-a-url", true},
		{"unresolvable host rejected", "http://no-such-host.invalid/feed.xml", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateURL(ctx, tc.url)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
		})
	}
}

// TestValidateURLRejectsRebindingToPrivateIP ensures a hostname that resolves
// to a private address is rejected even though the literal host isn't one.
func TestValidateURLRejectsRebindingToPrivateIP(t *testing.T) {
	fakeLookup(t, map[string][]net.IPAddr{
		"evil.example": {{IP: net.ParseIP("169.254.169.254")}},
	})
	if err := ValidateURL(context.Background(), "http://evil.example/feed.xml"); err == nil {
		t.Fatal("expected error for hostname resolving to a private/metadata IP")
	}
}

// TestDialFirstReachableSkipsUnreachableAddresses verifies that a resolved
// address which fails to connect (e.g. an AAAA record with no IPv6 egress,
// or one dead IP in a round-robin pool) doesn't sink the whole request: the
// dialer must move on to the next resolved address instead of only trying
// the first one and giving up.
func TestDialFirstReachableSkipsUnreachableAddresses(t *testing.T) {
	addrs := []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")}, // simulates an unreachable IPv6 address
		{IP: net.ParseIP("93.184.216.34")},
	}
	var tried []string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		tried = append(tried, addr)
		if addr == "93.184.216.34:443" {
			client, server := net.Pipe()
			server.Close()
			return client, nil
		}
		return nil, &net.OpError{Op: "dial", Err: net.UnknownNetworkError("unreachable")}
	}

	conn, err := dialFirstReachable(context.Background(), "tcp", addrs, "443", dial)
	if err != nil {
		t.Fatalf("expected fallback to the second address to succeed, got: %v", err)
	}
	conn.Close()
	if len(tried) != 2 {
		t.Fatalf("expected both addresses to be tried, got %v", tried)
	}
}

// TestDialFirstReachableReturnsLastErrorWhenAllFail ensures a real error
// surfaces (not a nil conn with nil err) when every resolved address is
// unreachable.
func TestDialFirstReachableReturnsLastErrorWhenAllFail(t *testing.T) {
	addrs := []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("2001:db8::2")},
	}
	dial := func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: net.UnknownNetworkError("unreachable")}
	}

	if _, err := dialFirstReachable(context.Background(), "tcp", addrs, "443", dial); err == nil {
		t.Fatal("expected an error when every resolved address fails")
	}
}

func TestSafeClientNoProxy(t *testing.T) {
	client, err := SafeClient(0, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("expected no proxy configured")
	}
}

func TestSafeClientAcceptsSupportedProxySchemes(t *testing.T) {
	for _, u := range []string{
		"http://proxy.example.com:3128",
		"https://proxy.example.com:3128",
		"socks5://proxy.example.com:1080",
		"socks5h://proxy.example.com:1080",
	} {
		t.Run(u, func(t *testing.T) {
			client, err := SafeClient(5, u)
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", u, err)
			}
			if client.Transport.(*http.Transport).Proxy == nil {
				t.Fatal("expected a proxy function to be configured")
			}
		})
	}
}

func TestSafeClientRejectsUnsupportedProxyScheme(t *testing.T) {
	if _, err := SafeClient(5, "ftp://proxy.example.com:21"); err == nil {
		t.Fatal("expected an error for an unsupported proxy scheme")
	}
}

func TestSafeClientRejectsMalformedProxyURL(t *testing.T) {
	if _, err := SafeClient(5, "://not-a-url"); err == nil {
		t.Fatal("expected an error for a malformed proxy_url")
	}
}

func TestSafeClientRejectsProxyWithoutHost(t *testing.T) {
	if _, err := SafeClient(5, "http://"); err == nil {
		t.Fatal("expected an error for proxy_url without a host")
	}
}

func TestIsPrivateHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"0.0.0.0", true},
		{"127.0.0.1", true},
		{"169.254.169.254", true},
		{"10.1.2.3", true},
		{"172.16.0.5", true},
		{"172.31.255.255", true},
		{"192.168.0.1", true},
		{"100.64.0.1", true},
		{"198.18.0.1", true},
		{"224.0.0.1", true},
		{"255.255.255.255", true},
		{"ff02::1", true},
		{"8.8.8.8", false},
		{"example.com", false}, // hostnames resolved separately, not literal IPs
	}
	for _, tc := range tests {
		if got := isPrivateHost(tc.host); got != tc.want {
			t.Errorf("isPrivateHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
