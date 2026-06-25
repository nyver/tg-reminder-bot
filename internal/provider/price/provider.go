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
	proxyURL   string
	log        *slog.Logger
	lookupIP   func(context.Context, string) ([]net.IPAddr, error)

	// Headless browser resources. Lazily initialized on first headless request.
	allocOnce   sync.Once
	allocCtx    context.Context
	allocCancel context.CancelFunc
}

func New(userAgent string, timeout time.Duration, headless bool, proxyURL string, log *slog.Logger) *Provider {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	jar, _ := cookiejar.New(nil)

	// safeDialContext resolves DNS and validates each resolved IP at connect time,
	// closing the DNS-rebinding window that exists when validation and dialing are
	// separate steps (SSRF prevention).
	safeDialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if isPrivateIPLiteral(host) {
			return nil, fmt.Errorf("connection to private host %q blocked", host)
		}
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
		cancel()
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no addresses resolved for %s", host)
		}
		for _, a := range addrs {
			if isPrivateIP(a.IP) {
				return nil, fmt.Errorf("connection to private IP %s (→%s) blocked", host, a.IP)
			}
		}
		// Connect directly to the first resolved IP so the OS cannot silently
		// re-resolve the hostname to a different (private) address mid-flight.
		d := net.Dialer{}
		return d.DialContext(ctx, network, net.JoinHostPort(addrs[0].IP.String(), port))
	}

	// Force HTTP/1.1 by disabling TLS ALPN upgrade to h2.
	// Many WAFs (including DNS shop's) fingerprint the HTTP/2 stream and reject
	// non-browser clients; HTTP/1.1 is harder to distinguish from a real browser.
	transport := &http.Transport{
		TLSNextProto:    make(map[string]func(string, *tls.Conn) http.RoundTripper),
		IdleConnTimeout: 30 * time.Second,
		DialContext:     safeDialContext,
	}
	if headless {
		log.Info("price: headless mode enabled — Chromium required in runtime")
	}
	if proxyURL != "" {
		log.Info("price: proxy configured", "proxy_url", redactURL(proxyURL))
	}
	return &Provider{
		httpClient: &http.Client{Timeout: timeout, Jar: jar, Transport: transport},
		userAgent:  userAgent,
		timeout:    timeout,
		headless:   headless,
		proxyURL:   proxyURL,
		log:        log,
		lookupIP:   net.DefaultResolver.LookupIPAddr,
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

// chromeBin is the official Google Chrome binary (not the wrapper script).
// Using the binary directly avoids the wrapper prepending extra flags that
// would override our --user-data-dir and break crashpad's --database path.
const chromeBin = "/opt/google/chrome/chrome"

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
			// Do NOT set --disable-gpu: it strips Canvas/WebGL support which
			// ServicePipe and similar WAFs use for browser fingerprinting.
			// SwiftShader provides software-based OpenGL/WebGL without a real
			// GPU and is bundled with Chrome 80+.
			chromedp.Flag("use-angle", "swiftshader"),
			chromedp.Flag("use-gl", "angle"),
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
		if p.proxyURL != "" {
			opts = append(opts, chromedp.ProxyServer(p.proxyURL))
		}
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

	// Headless mode needs extra time for WAF JS challenges (ServicePipe,
	// Cloudflare, Qrator). Use at least 30s regardless of the configured
	// HTTP timeout so challenge redirects have room to complete.
	headlessTimeout := p.timeout
	if headlessTimeout < 30*time.Second {
		headlessTimeout = 30 * time.Second
	}
	tabCtx, timeoutCancel := context.WithTimeout(tabCtx, headlessTimeout)
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
	var resolvedChallenge bool
	if err := chromedp.Run(tabCtx,
		// Enable CDP Network domain so events above are delivered.
		chromedp.ActionFunc(func(ctx context.Context) error {
			return network.Enable().Do(ctx)
		}),
		// Override UA + navigator.platform at the CDP emulation level.
		chromedp.ActionFunc(func(ctx context.Context) error {
			return emulation.SetUserAgentOverride(p.userAgent).
				WithPlatform("Win32").
				WithUserAgentMetadata(&emulation.UserAgentMetadata{
					Platform:        "Windows",
					PlatformVersion: "10.0.0",
					Architecture:    "x86",
					Bitness:         "64",
					Mobile:          false,
					Brands: []*emulation.UserAgentBrandVersion{
						{Brand: "Google Chrome", Version: "125"},
						{Brand: "Chromium", Version: "125"},
						{Brand: "Not.A/Brand", Version: "24"},
					},
					FullVersionList: []*emulation.UserAgentBrandVersion{
						{Brand: "Google Chrome", Version: "125.0.6422.112"},
						{Brand: "Chromium", Version: "125.0.6422.112"},
						{Brand: "Not.A/Brand", Version: "24.0.0.0"},
					},
				}).
				Do(ctx)
		}),
		// Force-inject Sec-CH-UA Client Hints and other headers that emulation
		// SetUserAgentOverride / UserAgentMetadata fails to propagate in headless
		// Chromium on Debian. Without Sec-CH-UA*, Qrator blocks the request
		// immediately as a non-browser client.
		chromedp.ActionFunc(func(ctx context.Context) error {
			return network.SetExtraHTTPHeaders(network.Headers{
				"Accept":             "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
				"Accept-Language":    "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7",
				"sec-ch-ua":          `"Google Chrome";v="125", "Chromium";v="125", "Not.A/Brand";v="24"`,
				"sec-ch-ua-mobile":   "?0",
				"sec-ch-ua-platform": `"Windows"`,
			}).Do(ctx)
		}),
		// Inject stealth patches before any page script runs.
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthScript).Do(ctx)
			return err
		}),
		chromedp.Navigate(rawURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Poll(`document.readyState === "complete"`, nil),
		// Handle WAF JS challenges (Qrator, Cloudflare, ServicePipe, etc.).
		// The challenge page runs JS, sets a validation cookie, then redirects
		// back to the original URL. We detect the challenge and wait for the
		// redirect + real page load before extracting HTML.
		// resolvedChallenge is set to true when we actually waited for one so
		// the subsequent XHR sleep can be skipped (time already spent).
		chromedp.ActionFunc(func(ctx context.Context) error {
			var isChallenge bool
			if err := chromedp.Evaluate(
				`!!document.querySelector('script[src*="__qrator"], #cf-challenge-running, .cf-browser-verification') `+
					`|| document.body.innerHTML.includes('servicepipe.ru') `+
					`|| (document.title === '' && document.body.innerHTML.length < 10000)`,
				&isChallenge,
			).Do(ctx); err != nil || !isChallenge {
				return nil // not a challenge page, continue
			}
			resolvedChallenge = true
			// Use a sub-context so the poll can time out without failing the whole
			// fetch — if the challenge doesn't resolve we proceed with whatever HTML
			// the tab currently holds (may yield no price, retried next tick).
			// Leave 3s for post-challenge HTML extraction.
			budget := 3 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				if rem := time.Until(deadline) - 3*time.Second; rem > budget {
					budget = rem
				}
			}
			challengeCtx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			err := chromedp.Poll(
				`!document.querySelector('script[src*="__qrator"], #cf-challenge-running, .cf-browser-verification') `+
					`&& !document.body.innerHTML.includes('servicepipe.ru') `+
					`&& document.title !== '' `+
					`&& document.readyState === "complete"`,
				nil,
			).Do(challengeCtx)
			if err != nil && challengeCtx.Err() != nil {
				return nil // challenge timed out gracefully
			}
			return err
		}),
		// Extra wait for XHR-rendered price widgets — skip when we already spent
		// time waiting for a WAF challenge redirect.
		chromedp.ActionFunc(func(ctx context.Context) error {
			if resolvedChallenge {
				return nil
			}
			return chromedp.Sleep(2 * time.Second).Do(ctx)
		}),
		// If we waited for a challenge but the page is still the spinner,
		// try navigating to the URL once more — the challenge script may have
		// silently set cookies without triggering a DOM redirect (ServicePipe
		// sometimes does this). A second request with valid cookies will load
		// the real page.
		chromedp.ActionFunc(func(ctx context.Context) error {
			if !resolvedChallenge {
				return nil
			}
			var stillChallenge bool
			if err := chromedp.Evaluate(
				`document.body.innerHTML.includes('servicepipe.ru') `+
					`|| (document.title === '' && document.body.innerHTML.length < 10000)`,
				&stillChallenge,
			).Do(ctx); err != nil || !stillChallenge {
				return nil
			}
			// Second attempt: cookies may now be set.
			if err := chromedp.Navigate(rawURL).Do(ctx); err != nil {
				return nil
			}
			_ = chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
			return chromedp.Poll(`document.readyState === "complete"`, nil).Do(ctx)
		}),
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
	if err := p.validateURL(ctx, rawURL); err != nil {
		return provider.Measurement{}, fmt.Errorf("price provider: %w", err)
	}

	body, httpStatus, err := p.fetchBody(ctx, rawURL)
	if err != nil {
		return provider.Measurement{Available: false, HTTPStatus: httpStatus}, err
	}
	if body == nil {
		return provider.Measurement{Available: false, HTTPStatus: httpStatus}, nil
	}

	kopecks, currency, pageTitle, found := extractPrice(body)
	if !found || kopecks <= 0 {
		logArgs := []any{
			"url", rawURL,
			"headless", p.headless,
			"page_title", extractPageTitle(body),
			"body_len", len(body),
		}
		if p.headless && len(body) < 10000 && bytes.Contains(body, []byte("servicepipe.ru")) {
			logArgs = append(logArgs, "hint", "ServicePipe WAF blocked headless request — configure proxy_url (residential proxy) to bypass")
		}
		p.log.Warn("price: extraction found nothing", logArgs...)
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

// fetchBody returns the page HTML and HTTP status.
// Returns (nil, status, nil) when the server responded with a non-success
// status that we treat as "temporarily unavailable" (401/403/404).
// Returns (nil, status, err) for other 4xx/5xx errors.
func (p *Provider) fetchBody(ctx context.Context, rawURL string) ([]byte, int, error) {
	if p.headless {
		body, err := p.fetchPageHeadless(rawURL)
		return body, 0, err
	}

	body, status, err := p.fetchPage(ctx, rawURL, "")
	if err != nil {
		return nil, 0, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Pre-warm the session by visiting the site root so the cookie jar
		// captures any session cookies the WAF sets, then retry with a Referer.
		rootURL := p.warmSession(ctx, rawURL)
		body, status, err = p.fetchPage(ctx, rawURL, rootURL)
		if err != nil {
			return nil, 0, err
		}
	}
	if status == http.StatusNotFound {
		return nil, status, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		p.log.Warn("price: access denied, will retry next tick", "url", rawURL, "status", status)
		return nil, status, nil
	}
	if status >= 400 {
		return nil, status, fmt.Errorf("price fetch: HTTP %d", status)
	}
	return body, status, nil
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
	// net.Error.Temporary() is deprecated since Go 1.18. Use Timeout() for the
	// generic case, and check net.DNSError.IsTemporary explicitly for DNS failures
	// (e.g. "server misbehaving" / SERVFAIL), which are worth retrying.
	var netErr net.Error
	if !errors.As(err, &netErr) {
		return false
	}
	if netErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsTemporary
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

// privateNets contains RFC-1918 + link-local + loopback ranges checked by isPrivateHost.
var privateNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // loopback
		"10.0.0.0/8",     // RFC-1918 class A
		"172.16.0.0/12",  // RFC-1918 class B (172.16–31)
		"192.168.0.0/16", // RFC-1918 class C
		"169.254.0.0/16", // link-local / AWS metadata (169.254.169.254)
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA
		"fe80::/10",      // IPv6 link-local
	} {
		_, network, _ := net.ParseCIDR(cidr)
		if network != nil {
			privateNets = append(privateNets, network)
		}
	}
}

// redactURL replaces the password in a proxy URL with "***" before logging.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPass := u.User.Password(); hasPass {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}

// --- URL validation ---

// validateURL checks the user-supplied URL for obvious abuse (bad scheme, private
// IP literals) and pre-resolves DNS to reject known-private targets early.
// A second, independent SSRF guard runs inside the transport's DialContext at
// TCP-connect time, closing the DNS-rebinding window between these two checks.
func (p *Provider) validateURL(ctx context.Context, raw string) error {
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
	if isPrivateIPLiteral(host) {
		return fmt.Errorf("private host not allowed: %s", host)
	}
	lookup := p.lookupIP
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := lookup(lookupCtx, host)
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

func isPrivateIPLiteral(host string) bool {
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

func isPrivateIP(ip net.IP) bool {
	for _, private := range privateNets {
		if private.Contains(ip) {
			return true
		}
	}
	return false
}
