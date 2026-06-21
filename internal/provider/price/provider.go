package price

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const providerType = "price"

// Provider implements provider.MetricProvider by scraping product pages.
// Extraction strategy: JSON-LD → OG meta → microdata → CSS heuristic.
type Provider struct {
	httpClient *http.Client
	userAgent  string
	log        *slog.Logger
}

func New(userAgent string, timeout time.Duration, log *slog.Logger) *Provider {
	return &Provider{
		httpClient: &http.Client{Timeout: timeout},
		userAgent:  userAgent,
		log:        log,
	}
}

func (p *Provider) Type() string { return providerType }

func (p *Provider) Sample(ctx context.Context, q provider.Query) (provider.Measurement, error) {
	rawURL := q.Params["url"]
	if rawURL == "" {
		return provider.Measurement{}, fmt.Errorf("price provider: url param required")
	}
	if err := validateURL(rawURL); err != nil {
		return provider.Measurement{}, fmt.Errorf("price provider: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return provider.Measurement{}, err
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return provider.Measurement{}, fmt.Errorf("price fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return provider.Measurement{Available: false}, nil
	}
	if resp.StatusCode >= 400 {
		return provider.Measurement{}, fmt.Errorf("price fetch: HTTP %d", resp.StatusCode)
	}

	// TODO M4: implement multi-strategy extraction with goquery.
	// JSON-LD → OG meta → microdata → CSS heuristic.
	// For now, return a stub so the evaluator compiles.
	p.log.Info("price: sampled (stub)", "url", rawURL)
	return provider.Measurement{
		Value:     0,
		Currency:  "RUB",
		Available: true,
		Title:     q.Title,
	}, nil
}

var rePriceNum = regexp.MustCompile(`[\d\s]+[,.]?\d*`)

// ParsePrice converts human-readable price strings to kopecks (int64).
// Handles: "3 190 ₽", "3190.50", "3 190,50 руб."
func ParsePrice(s string) (int64, string, error) {
	s = strings.TrimSpace(s)
	currency := "RUB"
	if strings.Contains(s, "$") || strings.Contains(s, "USD") {
		currency = "USD"
	}
	if strings.Contains(s, "€") || strings.Contains(s, "EUR") {
		currency = "EUR"
	}

	// Remove all non-numeric except dot/comma.
	clean := regexp.MustCompile(`[^\d,.]`).ReplaceAllString(s, "")
	clean = strings.ReplaceAll(clean, ",", ".")

	if clean == "" {
		return 0, "", fmt.Errorf("no numeric value in %q", s)
	}

	// Handle "3190.50" → 319050 kopecks.
	parts := strings.SplitN(clean, ".", 2)
	rubles, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", err
	}
	kopecks := rubles * 100
	if len(parts) == 2 {
		dec := parts[1]
		if len(dec) == 1 {
			dec += "0"
		}
		if len(dec) > 2 {
			dec = dec[:2]
		}
		k, _ := strconv.ParseInt(dec, 10, 64)
		kopecks += k
	}
	return kopecks, currency, nil
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	// SSRF protection: reject private IP ranges.
	host := u.Hostname()
	if isPrivateHost(host) {
		return fmt.Errorf("private host not allowed: %s", host)
	}
	return nil
}

func isPrivateHost(host string) bool {
	private := []string{"localhost", "127.", "10.", "192.168.", "172.16.", "::1", "0.0.0.0"}
	h := strings.ToLower(host)
	for _, p := range private {
		if strings.HasPrefix(h, p) {
			return true
		}
	}
	return false
}
