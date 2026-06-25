package price

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func publicLookup(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

func TestSampleRetriesTemporaryNetworkErrors(t *testing.T) {
	attempts := 0
	p := &Provider{
		httpClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			if attempts < maxFetchAttempts {
				return nil, &net.DNSError{Err: "temporary failure", Name: "shop.test", IsTemporary: true}
			}
			body := `<meta property="product:price:amount" content="3190"><meta property="product:price:currency" content="RUB">`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		userAgent: "test",
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		lookupIP:  publicLookup,
	}

	got, err := p.Sample(context.Background(), provider.Query{Params: map[string]string{"url": "https://shop.test/product"}})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != maxFetchAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, maxFetchAttempts)
	}
	if !got.Available || got.Value != 319000 || got.Currency != "RUB" {
		t.Fatalf("unexpected measurement: %+v", got)
	}
}

func TestSampleDoesNotRetryPermanentNetworkError(t *testing.T) {
	attempts := 0
	p := &Provider{
		httpClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return nil, errors.New("permanent failure")
		})},
		userAgent: "test",
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		lookupIP:  publicLookup,
	}

	_, err := p.Sample(context.Background(), provider.Query{Params: map[string]string{"url": "https://shop.test/product"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestSampleStopsRetryingWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Provider{
		httpClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			cancel()
			return nil, &net.DNSError{Err: "temporary failure", Name: "shop.test", IsTemporary: true}
		})},
		userAgent: "test",
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		lookupIP:  publicLookup,
	}

	started := time.Now()
	_, err := p.Sample(ctx, provider.Query{Params: map[string]string{"url": "https://shop.test/product"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if time.Since(started) >= retryBaseDelay {
		t.Fatal("cancellation did not stop retry delay")
	}
}

func TestValidateURLRejectsHostResolvingToPrivateIP(t *testing.T) {
	p := &Provider{
		lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
		},
	}

	err := p.validateURL(context.Background(), "https://metadata.example/product")
	if err == nil {
		t.Fatal("expected private resolved address to be rejected")
	}
	if !strings.Contains(err.Error(), "private resolved address") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateURLRejectsPrivateIPLiteral(t *testing.T) {
	p := &Provider{lookupIP: publicLookup}
	if err := p.validateURL(context.Background(), "http://127.0.0.1/product"); err == nil {
		t.Fatal("expected private IP literal to be rejected")
	}
}
