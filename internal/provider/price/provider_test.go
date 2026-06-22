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
