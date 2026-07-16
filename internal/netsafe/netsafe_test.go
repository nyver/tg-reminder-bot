package netsafe

import (
	"context"
	"net"
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
		{"8.8.8.8", false},
		{"example.com", false}, // hostnames resolved separately, not literal IPs
	}
	for _, tc := range tests {
		if got := isPrivateHost(tc.host); got != tc.want {
			t.Errorf("isPrivateHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
