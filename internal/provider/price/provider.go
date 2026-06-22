package price

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const (
	providerType     = "price"
	maxResponseBody  = 2 << 20 // 2 MB is enough for any product page
	maxFetchAttempts = 3
	retryBaseDelay   = 250 * time.Millisecond
)

// Provider implements provider.MetricProvider by scraping product pages.
// Extraction strategy: JSON-LD → OG meta → microdata.
type Provider struct {
	httpClient *http.Client
	userAgent  string
	log        *slog.Logger
}

func New(userAgent string, timeout time.Duration, log *slog.Logger) *Provider {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	jar, _ := cookiejar.New(nil)
	// Force HTTP/1.1 by disabling TLS ALPN upgrade to h2.
	// Many WAFs (including DNS shop's) fingerprint the HTTP/2 stream and reject
	// non-browser clients; HTTP/1.1 is harder to distinguish from a real browser.
	transport := &http.Transport{
		TLSNextProto:    make(map[string]func(string, *tls.Conn) http.RoundTripper),
		IdleConnTimeout: 30 * time.Second,
	}
	return &Provider{
		httpClient: &http.Client{Timeout: timeout, Jar: jar, Transport: transport},
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

	body, status, err := p.fetchPage(ctx, rawURL, "")
	if err != nil {
		return provider.Measurement{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Pre-warm the session by visiting the site root so the cookie jar
		// captures any session cookies the WAF sets, then retry with a Referer.
		rootURL := p.warmSession(ctx, rawURL)
		body, status, err = p.fetchPage(ctx, rawURL, rootURL)
		if err != nil {
			return provider.Measurement{}, err
		}
	}
	if status == http.StatusNotFound {
		return provider.Measurement{Available: false}, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Site is actively blocking this client (likely TLS fingerprint or IP reputation).
		// Mark as temporarily unavailable so the evaluator skips this tick and retries
		// next cycle rather than hard-failing the reminder.
		p.log.Warn("price: access denied, will retry next tick", "url", rawURL, "status", status)
		return provider.Measurement{Available: false}, nil
	}
	if status >= 400 {
		return provider.Measurement{}, fmt.Errorf("price fetch: HTTP %d", status)
	}

	kopecks, currency, pageTitle, found := extractPrice(body)
	if !found || kopecks <= 0 {
		p.log.Warn("price: extraction found nothing", "url", rawURL)
		return provider.Measurement{Available: true, Title: q.Title}, nil
	}

	title := q.Title
	if title == "" {
		title = pageTitle
	}

	p.log.Info("price: sampled", "url", rawURL, "kopecks", kopecks, "currency", currency, "title", title)
	return provider.Measurement{
		Value:     kopecks,
		Currency:  currency,
		Available: true,
		Title:     title,
	}, nil
}

// fetchPage performs a browser-like GET and returns (body, statusCode, error).
// referer, if non-empty, is sent as the Referer header and causes Sec-Fetch-Site
// to be set to "same-origin" (mimicking a navigation from the same domain).
func (p *Provider) fetchPage(ctx context.Context, rawURL, referer string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("sec-ch-ua", `"Google Chrome";v="125", "Chromium";v="125", "Not.A/Brand";v="24"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	if referer != "" {
		req.Header.Set("Referer", referer)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	} else {
		req.Header.Set("Sec-Fetch-Site", "none")
	}

	var resp *http.Response
	for attempt := 1; attempt <= maxFetchAttempts; attempt++ {
		resp, err = p.httpClient.Do(req)
		if err == nil {
			break
		}
		if ctx.Err() != nil || !isTemporaryNetworkError(err) || attempt == maxFetchAttempts {
			return nil, 0, fmt.Errorf("price fetch: %w", err)
		}

		delay := time.Duration(attempt) * retryBaseDelay
		p.log.Warn("price: temporary network error, retrying",
			"url", rawURL,
			"attempt", attempt,
			"retry_in_ms", delay.Milliseconds(),
			"err", err,
		)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, 0, fmt.Errorf("price fetch: %w", ctx.Err())
		case <-timer.C:
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		_, _ = io.Copy(io.Discard, resp.Body)
		if len(snippet) > 0 {
			p.log.Debug("price fetch non-200", "status", resp.StatusCode, "url", rawURL, "snippet", string(snippet))
		}
		return nil, resp.StatusCode, nil
	}

	bodyReader := io.Reader(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, resp.StatusCode, fmt.Errorf("price gzip: %w", err)
		}
		defer gr.Close()
		bodyReader = gr
	}

	body, err := io.ReadAll(io.LimitReader(bodyReader, maxResponseBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("price read: %w", err)
	}
	return body, resp.StatusCode, nil
}

func isTemporaryNetworkError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

// warmSession visits the site root so the cookie jar captures any WAF session
// cookies. Returns the root URL so the caller can use it as a Referer.
func (p *Provider) warmSession(ctx context.Context, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	rootURL := u.Scheme + "://" + u.Host + "/"
	_, _, _ = p.fetchPage(ctx, rootURL, "")
	return rootURL
}

// --- price extraction ---

// extractPrice tries three strategies in order and returns on first success.
func extractPrice(body []byte) (kopecks int64, currency, title string, ok bool) {
	if p, c, t, found := extractJSONLD(body); found {
		return p, c, t, true
	}
	if p, c, found := extractOGMeta(body); found {
		return p, c, extractPageTitle(body), true
	}
	if p, c, found := extractMicrodata(body); found {
		return p, c, extractPageTitle(body), true
	}
	return 0, "", "", false
}

// --- Strategy 1: JSON-LD ---

var reJSONLDBlock = regexp.MustCompile(`(?is)<script[^>]+type=["']application/ld\+json["'][^>]*>(.*?)</script>`)

// jsonLDNode is a partial decode of any JSON-LD object.
type jsonLDNode struct {
	Type   string          `json:"@type"`
	Name   string          `json:"name"`
	Offers json.RawMessage `json:"offers"`
	Graph  []jsonLDNode    `json:"@graph"`
}

type jsonLDOffer struct {
	Price         json.Number `json:"price"`
	PriceCurrency string      `json:"priceCurrency"`
}

func extractJSONLD(body []byte) (kopecks int64, currency, title string, ok bool) {
	for _, m := range reJSONLDBlock.FindAllSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(string(m[1]))

		// May be a single object or an array.
		nodes := parseJSONLDNodes([]byte(raw))
		for _, node := range nodes {
			p, c, t, found := priceFromNode(node)
			if found {
				return p, c, t, true
			}
		}
	}
	return 0, "", "", false
}

func parseJSONLDNodes(data []byte) []jsonLDNode {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return nil
	}
	if data[0] == '[' {
		var arr []jsonLDNode
		_ = json.Unmarshal(data, &arr)
		return arr
	}
	var node jsonLDNode
	if err := json.Unmarshal(data, &node); err != nil {
		return nil
	}
	if len(node.Graph) > 0 {
		return node.Graph
	}
	return []jsonLDNode{node}
}

func priceFromNode(node jsonLDNode) (kopecks int64, currency, title string, ok bool) {
	t := strings.ToLower(node.Type)
	if t != "product" && t != "https://schema.org/product" {
		return 0, "", "", false
	}
	if node.Offers == nil {
		return 0, "", "", false
	}

	// offers can be a single object or an array.
	var offers []jsonLDOffer
	if len(node.Offers) > 0 && node.Offers[0] == '[' {
		_ = json.Unmarshal(node.Offers, &offers)
	} else {
		var single jsonLDOffer
		if err := json.Unmarshal(node.Offers, &single); err == nil {
			offers = []jsonLDOffer{single}
		}
	}

	for _, offer := range offers {
		priceStr := offer.Price.String()
		if priceStr == "" || priceStr == "<nil>" {
			continue
		}
		p, _, err := ParsePrice(priceStr)
		if err != nil || p <= 0 {
			continue
		}
		curr := offer.PriceCurrency
		if curr == "" {
			curr = "RUB"
		}
		return p, curr, node.Name, true
	}
	return 0, "", "", false
}

// --- Strategy 2: OG / product meta tags ---

var (
	reMetaContent  = regexp.MustCompile(`(?i)<meta[^>]+property=["']([^"']+)["'][^>]+content=["']([^"']+)["']`)
	reMetaContent2 = regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"']+)["'][^>]+property=["']([^"']+)["']`)
	reMetaName     = regexp.MustCompile(`(?i)<meta[^>]+name=["']([^"']+)["'][^>]+content=["']([^"']+)["']`)
)

func extractOGMeta(body []byte) (kopecks int64, currency string, ok bool) {
	metas := collectMetas(body)
	amount, hasAmount := metas["product:price:amount"]
	if !hasAmount {
		amount, hasAmount = metas["og:price:amount"]
	}
	if !hasAmount {
		return 0, "", false
	}
	curr := metas["product:price:currency"]
	if curr == "" {
		curr = metas["og:price:currency"]
	}
	if curr == "" {
		curr = "RUB"
	}
	p, _, err := ParsePrice(amount + " " + curr)
	if err != nil || p <= 0 {
		return 0, "", false
	}
	return p, curr, true
}

func collectMetas(body []byte) map[string]string {
	out := make(map[string]string)
	for _, re := range []*regexp.Regexp{reMetaContent, reMetaContent2} {
		for _, m := range re.FindAllSubmatch(body, -1) {
			if len(m) >= 3 {
				key := strings.ToLower(string(m[1]))
				val := string(m[2])
				if re == reMetaContent2 {
					// swapped order: content first, then property
					key = strings.ToLower(string(m[2]))
					val = string(m[1])
				}
				if _, exists := out[key]; !exists {
					out[key] = val
				}
			}
		}
	}
	for _, m := range reMetaName.FindAllSubmatch(body, -1) {
		if len(m) >= 3 {
			key := strings.ToLower(string(m[1]))
			if _, exists := out[key]; !exists {
				out[key] = string(m[2])
			}
		}
	}
	return out
}

// --- Strategy 3: microdata itemprop="price" ---

// Matches both attribute orders: itemprop="price" content="..." and reversed.
var (
	reMicrodataPC = regexp.MustCompile(`(?i)itemprop=["']price["'][^>]+content=["']([^"']+)["']`)
	reMicrodataCP = regexp.MustCompile(`(?i)content=["']([^"']+)["'][^>]*itemprop=["']price["']`)
)

func extractMicrodata(body []byte) (kopecks int64, currency string, ok bool) {
	for _, re := range []*regexp.Regexp{reMicrodataPC, reMicrodataCP} {
		if m := re.FindSubmatch(body); m != nil {
			p, _, err := ParsePrice(string(m[1]))
			if err == nil && p > 0 {
				return p, "RUB", true
			}
		}
	}
	return 0, "", false
}

var reNonNumeric = regexp.MustCompile(`[^\d,.]`)

// --- page title ---

var reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func extractPageTitle(body []byte) string {
	if m := reTitle.FindSubmatch(body); m != nil {
		t := strings.TrimSpace(string(m[1]))
		// Strip common suffixes like " — DNS" or " | DNS-Shop"
		for _, sep := range []string{" — ", " | ", " - "} {
			if i := strings.LastIndex(t, sep); i > 0 {
				t = strings.TrimSpace(t[:i])
				break
			}
		}
		return t
	}
	return ""
}

// --- ParsePrice ---

// ParsePrice converts human-readable price strings to kopecks (int64).
// Handles: "3 190 ₽", "3190.50", "3 190,50 руб.", "9999.00 RUB".
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
	clean := reNonNumeric.ReplaceAllString(s, "")
	clean = strings.ReplaceAll(clean, ",", ".")

	if clean == "" {
		return 0, "", fmt.Errorf("no numeric value in %q", s)
	}

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

// --- URL validation ---

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	if isPrivateHost(u.Hostname()) {
		return fmt.Errorf("private host not allowed: %s", u.Hostname())
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
