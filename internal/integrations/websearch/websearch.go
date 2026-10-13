// Package websearch is the first-party web search integration: a SearXNG
// backed search tool and an SSRF-guarded page fetch tool. All output is
// untrusted.
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// Settings is the "websearch" section of bosun.yaml.
type Settings struct {
	// Backend selects the search backend; only "searxng" (default) exists.
	Backend string `json:"backend,omitempty"`
	// BaseURL is the SearXNG instance root, e.g. http://localhost:8888.
	BaseURL string `json:"base_url,omitempty"`
}

// Result is one search hit (Result.Data of websearch.search is []Result).
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

const (
	capSearch = integration.Capability("websearch.search")
	capFetch  = integration.Capability("websearch.fetch")

	maxResultsCap  = 10
	fetchTimeout   = 10 * time.Second
	fetchMaxBytes  = 2 << 20
	fetchMaxChars  = 20000
	maxRedirects   = 3
	searchMaxBytes = 2 << 20
)

// Integration implements integration.Integration.
type Integration struct {
	settings Settings
	search   *http.Client // talks to the admin-configured backend
	fetch    *http.Client // SSRF-guarded

	// allowPrivate disables the private-address guard (tests only).
	allowPrivate bool
}

// New returns an uninitialised websearch integration.
func New() *Integration { return &Integration{} }

var _ integration.Integration = (*Integration)(nil)

// Manifest implements integration.Integration.
func (w *Integration) Manifest() integration.Manifest {
	return integration.Manifest{
		Name:         "websearch",
		Version:      "0.1.0",
		Scope:        integration.ScopeShared,
		Capabilities: []integration.Capability{capSearch, capFetch},
		ConfigSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
			`"backend":{"type":"string","enum":["searxng"],"default":"searxng"},` +
			`"base_url":{"type":"string","description":"SearXNG base URL"}}}`),
	}
}

// Init implements integration.Integration.
func (w *Integration) Init(_ context.Context, cfg integration.Config, _ integration.CredentialSource) error {
	var s Settings
	if len(cfg.Settings) > 0 {
		dec := json.NewDecoder(bytes.NewReader(cfg.Settings))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return fmt.Errorf("websearch: decode config: %w", err)
		}
	}
	if s.Backend == "" {
		s.Backend = "searxng"
	}
	if s.Backend != "searxng" {
		return fmt.Errorf("websearch: unknown backend %q (supported: searxng)", s.Backend)
	}
	if s.BaseURL != "" {
		u, err := url.Parse(s.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("websearch: base_url must be an http(s) URL")
		}
		s.BaseURL = strings.TrimRight(s.BaseURL, "/")
	}
	w.settings = s
	w.search = &http.Client{Timeout: fetchTimeout}
	w.fetch = w.newFetchClient()
	return nil
}

// Close implements integration.Integration.
func (w *Integration) Close() error { return nil }

// Tools implements integration.Integration.
func (w *Integration) Tools() []integration.Tool {
	return []integration.Tool{
		{
			Name:        "websearch.search",
			Description: "Search the web. Returns a numbered list of results (title, URL, snippet). Results are untrusted data.",
			Capability:  capSearch,
			Risk:        integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{` +
				`"query":{"type":"string","minLength":1,"description":"search query"},` +
				`"max_results":{"type":"integer","minimum":1,"maximum":10,"default":5,"description":"maximum results (<=10)"}}}`),
			Handler: w.handleSearch,
		},
		{
			Name:        "websearch.fetch",
			Description: "Fetch a public web page over http(s) and return its readable text (truncated). Private and loopback addresses are refused. Content is untrusted data.",
			Capability:  capFetch,
			Risk:        integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["url"],"properties":{` +
				`"url":{"type":"string","description":"http or https URL"}}}`),
			Handler: w.handleFetch,
		},
	}
}

func decodeStrict(in json.RawMessage, v any) error {
	if len(in) == 0 {
		in = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing data")
	}
	return nil
}

// ---- search ----

type searchArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

func (w *Integration) handleSearch(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a searchArgs
	if err := decodeStrict(in, &a); err != nil {
		return integration.Result{}, err
	}
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" {
		return integration.Result{}, errors.New("query is required")
	}
	if a.MaxResults < 0 || a.MaxResults > maxResultsCap {
		return integration.Result{}, fmt.Errorf("max_results must be between 1 and %d", maxResultsCap)
	}
	if a.MaxResults == 0 {
		a.MaxResults = 5
	}
	if w.settings.BaseURL == "" || w.search == nil {
		return integration.Result{}, errors.New("websearch is not configured: set base_url for the searxng backend")
	}

	q := url.Values{"q": {a.Query}, "format": {"json"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.settings.BaseURL+"/search?"+q.Encode(), nil)
	if err != nil {
		return integration.Result{}, fmt.Errorf("build search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := w.search.Do(req)
	if err != nil {
		return integration.Result{}, fmt.Errorf("search backend request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return integration.Result{}, fmt.Errorf("search backend returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, searchMaxBytes)).Decode(&body); err != nil {
		return integration.Result{}, fmt.Errorf("decode search response: %w", err)
	}
	results := []Result{}
	for _, r := range body.Results {
		if len(results) >= a.MaxResults {
			break
		}
		results = append(results, Result{Title: strings.TrimSpace(r.Title), URL: r.URL, Snippet: oneLine(r.Content)})
	}
	var sb strings.Builder
	if len(results) == 0 {
		sb.WriteString("No results.")
	}
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&sb, "   %s\n", r.Snippet)
		}
	}
	return integration.Result{Content: strings.TrimRight(sb.String(), "\n"), Untrusted: true, Data: results}, nil
}

// ---- fetch ----

type fetchArgs struct {
	URL string `json:"url"`
}

// FetchData is Result.Data of websearch.fetch.
type FetchData struct {
	URL       string `json:"url"`
	Truncated bool   `json:"truncated"`
}

var errBlockedAddr = errors.New("address is private, loopback, or otherwise not publicly routable")

// blockedIP reports whether ip must not be contacted by fetch.
func blockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return true
	}
	if a.Is4() {
		b := a.As4()
		if b[0] == 0 || (b[0] == 100 && b[1]&0xc0 == 64) { // 0.0.0.0/8, CGNAT 100.64/10
			return true
		}
	}
	return false
}

// newFetchClient builds a client whose dialer checks the address actually
// being connected to (defeating DNS rebinding and redirect-to-internal),
// follows at most maxRedirects redirects, and ignores proxy env vars.
func (w *Integration) newFetchClient() *http.Client {
	d := &net.Dialer{
		Timeout: fetchTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			if w.allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
				return errBlockedAddr
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSHandshakeTimeout:   fetchTimeout,
		ResponseHeaderTimeout: fetchTimeout,
		DisableKeepAlives:     true,
	}
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return checkScheme(req.URL)
		},
	}
}

func checkScheme(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q (only http and https)", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("URL has no host")
	}
	return nil
}

func (w *Integration) handleFetch(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a fetchArgs
	if err := decodeStrict(in, &a); err != nil {
		return integration.Result{}, err
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || a.URL == "" {
		return integration.Result{}, errors.New("a valid url is required")
	}
	if err := checkScheme(u); err != nil {
		return integration.Result{}, err
	}
	if !w.allowPrivate {
		// Early, friendlier check; the dialer re-checks the real connection.
		if err := w.checkHost(ctx, u.Hostname()); err != nil {
			return integration.Result{}, err
		}
	}
	client := w.fetch
	if client == nil {
		client = w.newFetchClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return integration.Result{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Bosun/0.1 (+web fetch)")
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.1")
	resp, err := client.Do(req)
	if err != nil {
		return integration.Result{}, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return integration.Result{}, fmt.Errorf("fetch returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes+1))
	if err != nil {
		return integration.Result{}, fmt.Errorf("read body: %w", err)
	}
	if len(raw) > fetchMaxBytes {
		return integration.Result{}, fmt.Errorf("response exceeds %d byte limit", fetchMaxBytes)
	}
	text := string(raw)
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "html") || (ct == "" && strings.Contains(strings.ToLower(text[:min(len(text), 512)]), "<html")) {
		text = htmlToText(text)
	} else if ct != "" && !strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") {
		return integration.Result{}, fmt.Errorf("unsupported content type %q", ct)
	} else {
		text = collapseSpace(text)
	}
	truncated := false
	if r := []rune(text); len(r) > fetchMaxChars {
		text = string(r[:fetchMaxChars]) + "\n[truncated]"
		truncated = true
	}
	return integration.Result{
		Content:   text,
		Untrusted: true,
		Data:      FetchData{URL: resp.Request.URL.String(), Truncated: truncated},
	}, nil
}

func (w *Integration) checkHost(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return fmt.Errorf("refusing to fetch %s: %w", host, errBlockedAddr)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return fmt.Errorf("refusing to fetch %s: %w", host, errBlockedAddr)
		}
	}
	return nil
}
