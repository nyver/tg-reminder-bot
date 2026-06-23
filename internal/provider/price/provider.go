package price

import (
	"bytes"
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
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
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
// When headless=true a real Chromium browser is used, bypassing TLS-fingerprint WAFs.
type Provider struct {
	httpClient *http.Client
	userAgent  string
	timeout    time.Duration
	headless   bool
	log        *slog.Logger

	// Headless browser resources. Lazily initialized on first headless request.
	allocOnce   sync.Once
	allocCtx    context.Context
	allocCancel context.CancelFunc
}

func New(userAgent string, timeout time.Duration, headless bool, log *slog.Logger) *Provider {
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
	if headless {
		log.Info("price: headless mode enabled — Chromium required in runtime")
	}
	return &Provider{
		httpClient: &http.Client{Timeout: timeout, Jar: jar, Transport: transport},
		userAgent:  userAgent,
		timeout:    timeout,
		headless:   headless,
		log:        log,
	}
}

// Close releases the headless browser allocator (if started).
func (p *Provider) Close() {
	if p.allocCancel != nil {
		p.allocCancel()
	}
}

// chromeDataDir is the fixed user-data-dir for the headless Chrome process.
const chromeDataDir = "/tmp/chromium-data"

// chromeBin is the actual Chromium binary on Debian (not the wrapper script).
// The wrapper /usr/bin/chromium appends --user-data-dir=${HOME}/.config/chromium
// after our flags, overriding our value and causing crashpad to receive an
// empty --database path. Calling the binary directly avoids this.
const chromeBin = "/usr/lib/chromium/chromium"

// initAlloc lazily starts the Chromium exec allocator (shared Chrome process).
func (p *Provider) initAlloc() {
	p.allocOnce.Do(func() {
		// Pre-create both the user-data-dir and the crashpad database dir so
		// chrome_crashpad_handler always receives a non-empty --database path.
		_ = os.MkdirAll(chromeDataDir+"/Crash Reports", 0o755)

		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			// Bypass the Debian wrapper script that would override --user-data-dir.
			chromedp.ExecPath(chromeBin),
			chromedp.NoSandbox,
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-zygote", true),
			chromedp.Flag("user-data-dir", chromeDataDir),
			// Disable automation markers so JS-based WAFs cannot distinguish
			// this browser from a real user session.
			chromedp.Flag("enable-automation", false),
			chromedp.Flag("disable-blink-features", "AutomationControlled"),
			chromedp.UserAgent(p.userAgent),
			// The service user has no home dir; set HOME so Chrome can write
			// any remaining HOME-relative paths to a writable location.
			chromedp.Env("HOME=/tmp"),
		)
		p.allocCtx, p.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
		p.log.Info("price: headless chrome allocator ready")
	})
}

// stealthScript patches known Chromium automation fingerprints before any page
// script runs. Covers: webdriver flag, chrome.runtime, plugins, languages,
// permissions API — all common signals used by Qrator/Cloudflare bot detection.
const stealthScript = `(function(){
  Object.defineProperty(navigator,'webdriver',{get:()=>undefined});
  if(!window.chrome){window.chrome={runtime:{},loadTimes:function(){},csi:function(){},app:{}};}
  Object.defineProperty(navigator,'plugins',{get:()=>{const a=[1,2,3,4,5];a.item=i=>a[i];a.namedItem=()=>null;a.refresh=()=>{};return a;}});
  Object.defineProperty(navigator,'languages',{get:()=>['ru-RU','ru','en-US','en']});
  if(navigator.permissions&&navigator.permissions.query){
    const orig=navigator.permissions.query.bind(navigator.permissions);
    navigator.permissions.query=p=>p.name==='notifications'?Promise.resolve({state:'denied',onchange:null}):orig(p);
  }
})();`

// fetchPageHeadless opens a new browser tab, navigates to rawURL, and returns
// the full page HTML. The Chrome process is shared across calls (started once).
func (p *Provider) fetchPageHeadless(rawURL string) ([]byte, error) {
	p.initAlloc()

	// Each request gets its own tab; the underlying Chrome process is shared.
	tabCtx, cancel := chromedp.NewContext(p.allocCtx,
		chromedp.WithLogf(func(string, ...interface{}) {}), // suppress CDP noise
	)
	defer cancel()

	tabCtx, timeoutCancel := context.WithTimeout(tabCtx, p.timeout)
	defer timeoutCancel()

	p.log.Debug("price: headless fetch", "url", rawURL)

	// Capture real HTTP request/response headers via CDP Network domain.
	chromedp.ListenTarget(tabCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			if e.Type == network.ResourceTypeDocument {
				p.log.Debug("price: headless request",
					"url", e.Request.URL,
					"method", e.Request.Method,
					"req_headers", e.Request.Headers,
				)
			}
		case *network.EventResponseReceived:
			if e.Type == network.ResourceTypeDocument {
				p.log.Debug("price: headless response headers",
					"url", e.Response.URL,
					"status", e.Response.Status,
					"resp_headers", e.Response.Headers,
				)
			}
		}
	})

	var html string
	if err := chromedp.Run(tabCtx,
		// Enable CDP Network domain so events above are delivered.
		chromedp.ActionFunc(func(ctx context.Context) error {
			return network.Enable().Do(ctx)
		}),
		// Override UA at the CDP level: fixes the Linux-platform vs Windows-UA
		// mismatch that many WAFs detect via Client Hints headers.
		chromedp.ActionFunc(func(ctx context.Context) error {
			return emulation.SetUserAgentOverride(p.userAgent).
				WithAcceptLanguage("ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7").
				WithPlatform("Win32").
				Do(ctx)
		}),
		// Inject stealth patches before any page script runs.
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthScript).Do(ctx)
			return err
		}),
		chromedp.Navigate(rawURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Poll(`document.readyState === "complete"`, nil),
		// Handle WAF JS challenges (Qrator, Cloudflare, etc.).
		// The challenge page runs JS, sets a validation cookie, then redirects
		// back to the original URL. We detect the challenge and wait for the
		// redirect + real page load before extracting HTML.
		chromedp.ActionFunc(func(ctx context.Context) error {
			var isChallenge bool
			if err := chromedp.Evaluate(
				`!!document.querySelector('script[src*="__qrator"], #cf-challenge-running, .cf-browser-verification')`,
				&isChallenge,
			).Do(ctx); err != nil || !isChallenge {
				return nil // not a challenge page, continue
			}
			// Poll until the challenge redirects and the real page is loaded.
			return chromedp.Poll(
				`!document.querySelector('script[src*="__qrator"], #cf-challenge-running, .cf-browser-verification') `+
					`&& document.readyState === "complete"`,
				nil,
			).Do(ctx)
		}),
		// Extra wait for XHR-rendered price widgets after full page load.
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`document.documentElement.outerHTML`, &html),
	); err != nil {
		return nil, fmt.Errorf("headless fetch: %w", err)
	}

	const bodySnippetLen = 512
	snippet := html
	if len(snippet) > bodySnippetLen {
		snippet = snippet[:bodySnippetLen]
	}
	p.log.Debug("price: headless page loaded",
		"url", rawURL,
		"page_title", extractPageTitle([]byte(html)),
		"html_len", len(html),
		"html_snippet", snippet,
	)
	return []byte(html), nil
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

	body, err := p.fetchBody(ctx, rawURL)
	if err != nil {
		return provider.Measurement{}, err
	}
	if body == nil {
		// HTTP path returned a non-200 that we treat as "unavailable".
		return provider.Measurement{Available: false}, nil
	}

	kopecks, currency, pageTitle, found := extractPrice(body)
	if !found || kopecks <= 0 {
		p.log.Warn("price: extraction found nothing",
			"url", rawURL,
			"headless", p.headless,
			"page_title", extractPageTitle(body),
			"body_len", len(body),
		)
		return provider.Measurement{Available: true, Title: q.Title}, nil
	}

	title := q.Title
	if title == "" {
		title = pageTitle
	}

	p.log.Info("price: sampled", "url", rawURL, "kopecks", kopecks, "currency", currency, "title", title, "headless", p.headless)
	return provider.Measurement{
		Value:     kopecks,
		Currency:  currency,
		Available: true,
		Title:     title,
	}, nil
}

// fetchBody returns the page HTML. Returns (nil, nil) when the page is
// temporarily unavailable (401/403/404) so the caller can return Available=false
// without treating it as an error.
func (p *Provider) fetchBody(ctx context.Context, rawURL string) ([]byte, error) {
	if p.headless {
		return p.fetchPageHeadless(rawURL)
	}

	body, status, err := p.fetchPage(ctx, rawURL, "")
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Pre-warm the session by visiting the site root so the cookie jar
		// captures any session cookies the WAF sets, then retry with a Referer.
		rootURL := p.warmSession(ctx, rawURL)
		body, status, err = p.fetchPage(ctx, rawURL, rootURL)
		if err != nil {
			return nil, err
		}
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		p.log.Warn("price: access denied, will retry next tick", "url", rawURL, "status", status)
		return nil, nil
	}
	if status >= 400 {
		return nil, fmt.Errorf("price fetch: HTTP %d", status)
	}
	return body, nil
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

	p.log.Debug("price: request",
		"url", rawURL,
		"referer", referer,
		"req_headers", req.Header,
	)

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

	p.log.Debug("price: response",
		"url", rawURL,
		"status", resp.StatusCode,
		"resp_headers", resp.Header,
	)

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		_, _ = io.Copy(io.Discard, resp.Body)
		p.log.Debug("price fetch non-200",
			"status", resp.StatusCode,
			"url", rawURL,
			"resp_headers", resp.Header,
			"snippet", string(snippet),
		)
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

// extractPrice tries four strategies in order and returns on first success:
// JSON-LD → OG meta → microdata → script JSON (DataLayer / inline state).
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
	if p, c, found := extractScriptJSON(body); found {
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
	if t != "product" && t != "https://schema.org/product" && t != "http://schema.org/product" {
		return 0, "", "", false
	}
	if node.Offers == nil {
		return 0, "", "", false
	}

	// offers can be a single object or an array; trim whitespace before check.
	var offers []jsonLDOffer
	trimmed := bytes.TrimSpace(node.Offers)
	if len(trimmed) > 0 && trimmed[0] == '[' {
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

// --- Strategy 4: script JSON (DataLayer, inline state, custom JSON blobs) ---

// reScriptBlock matches any <script> tag content (not just ld+json).
var reScriptBlock = regexp.MustCompile(`(?is)<script[^>]*>(.*?)</script>`)

// rePriceKV matches common price key–value patterns in JSON or JS literals:
//
//	"price": 12990       "price":"12990.50"
//	"currentPrice":9990  "salePrice" : "8 990"
var rePriceKV = regexp.MustCompile(`(?i)"(?:price|currentPrice|salePrice|actualPrice|priceValue|basePrice)"\s*:\s*"?([\d][\d\s.,]*)`)

func extractScriptJSON(body []byte) (kopecks int64, currency string, ok bool) {
	for _, m := range reScriptBlock.FindAllSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		for _, pm := range rePriceKV.FindAllSubmatch(m[1], -1) {
			if len(pm) < 2 {
				continue
			}
			p, _, err := ParsePrice(string(pm[1]))
			// Sanity-check: 10 RUB – 10 000 000 RUB (1 000 – 1 000 000 000 kopecks).
			if err != nil || p < 1_000 || p > 1_000_000_000 {
				continue
			}
			return p, "RUB", true
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
