package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dperedo/bosun/internal/integration"
)

func newWS(t *testing.T, settings string, allowPrivate bool) *Integration {
	t.Helper()
	w := New()
	w.allowPrivate = allowPrivate
	if err := w.Init(context.Background(), integration.Config{Name: "websearch", Settings: json.RawMessage(settings)}, nil); err != nil {
		t.Fatal(err)
	}
	return w
}

func tool(t *testing.T, w *Integration, name string) integration.Tool {
	t.Helper()
	for _, tl := range w.Tools() {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return integration.Tool{}
}

func TestRegisters(t *testing.T) {
	if err := integration.NewRegistry().Register(New()); err != nil {
		t.Fatal(err)
	}
}

func TestSearch(t *testing.T) {
	var gotQ, gotFmt string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(rw, r)
			return
		}
		gotQ, gotFmt = r.URL.Query().Get("q"), r.URL.Query().Get("format")
		fmt.Fprint(rw, `{"results":[
		 {"title":"Go","url":"https://go.dev","content":"The  Go\nlanguage"},
		 {"title":"Two","url":"https://two.example","content":""},
		 {"title":"Three","url":"https://three.example","content":"x"}]}`)
	}))
	defer srv.Close()
	w := newWS(t, fmt.Sprintf(`{"backend":"searxng","base_url":%q}`, srv.URL+"/"), false)
	h := tool(t, w, "websearch.search").Handler

	res, err := h(context.Background(), json.RawMessage(`{"query":"golang & more","max_results":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if gotQ != "golang & more" || gotFmt != "json" {
		t.Errorf("q=%q format=%q", gotQ, gotFmt)
	}
	if !res.Untrusted {
		t.Error("not untrusted")
	}
	data := res.Data.([]Result)
	if len(data) != 2 || data[0].Snippet != "The Go language" {
		t.Errorf("data = %+v", data)
	}
	if !strings.Contains(res.Content, "1. Go\n   https://go.dev\n   The Go language") || !strings.Contains(res.Content, "2. Two") || strings.Contains(res.Content, "Three") {
		t.Errorf("content:\n%s", res.Content)
	}

	for _, bad := range []string{`{}`, `{"query":"x","max_results":11}`, `{"query":"x","extra":1}`, `not json`} {
		if _, err := h(context.Background(), json.RawMessage(bad)); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}

func TestSearchBackendErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(500) }))
	defer srv.Close()
	w := newWS(t, fmt.Sprintf(`{"base_url":%q}`, srv.URL), false)
	if _, err := tool(t, w, "websearch.search").Handler(context.Background(), json.RawMessage(`{"query":"x"}`)); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v", err)
	}
	w = newWS(t, ``, false)
	if _, err := tool(t, w, "websearch.search").Handler(context.Background(), json.RawMessage(`{"query":"x"}`)); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err = %v", err)
	}
	if err := New().Init(context.Background(), integration.Config{Settings: json.RawMessage(`{"backend":"bing"}`)}, nil); err == nil {
		t.Error("unknown backend accepted")
	}
	if err := New().Init(context.Background(), integration.Config{Settings: json.RawMessage(`{"nope":1}`)}, nil); err == nil {
		t.Error("unknown setting accepted")
	}
}

func fetch(w *Integration, u string) (integration.Result, error) {
	b, _ := json.Marshal(map[string]string{"url": u})
	return w.fetchTool().Handler(context.Background(), b)
}

func (w *Integration) fetchTool() integration.Tool { return w.Tools()[1] }

func TestFetchHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(rw, `<html><head><title>T</title><style>p{color:red}</style><script>alert("x")</script></head>
<body><!-- hidden --><h1>Hello &amp; welcome</h1><p>One   <b>two</b></p><p>Three</p>
<SCRIPT type="x">evil()</SCRIPT></body></html>`)
	}))
	defer srv.Close()
	w := newWS(t, `{}`, true)
	res, err := fetch(w, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Untrusted {
		t.Error("not untrusted")
	}
	want := "T\nHello & welcome\nOne two\nThree"
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
}

func TestHTMLToText(t *testing.T) {
	got := htmlToText("a<br>b <i>c</i>d &lt;3 <notclosed")
	if got != "a\nb c d <3" {
		t.Errorf("got %q", got)
	}
}

func TestFetchTruncatesAndCaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain")
		switch r.URL.Path {
		case "/long":
			fmt.Fprint(rw, strings.Repeat("a", 50000))
		case "/huge":
			fmt.Fprint(rw, strings.Repeat("a", fetchMaxBytes+10))
		case "/bin":
			rw.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(rw, "xx")
		case "/404":
			http.NotFound(rw, r)
		}
	}))
	defer srv.Close()
	w := newWS(t, `{}`, true)
	res, err := fetch(w, srv.URL+"/long")
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(res.Content)) > fetchMaxChars+20 || !res.Data.(FetchData).Truncated || !strings.HasSuffix(res.Content, "[truncated]") {
		t.Errorf("len=%d data=%+v", len(res.Content), res.Data)
	}
	if _, err := fetch(w, srv.URL+"/huge"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("huge: %v", err)
	}
	if _, err := fetch(w, srv.URL+"/bin"); err == nil {
		t.Error("binary accepted")
	}
	if _, err := fetch(w, srv.URL+"/404"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
}

func TestFetchRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/r/", func(rw http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(r.URL.Path, "/r/%d", &n)
		if n == 0 {
			rw.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(rw, "arrived")
			return
		}
		http.Redirect(rw, r, fmt.Sprintf("/r/%d", n-1), http.StatusFound)
	})
	mux.HandleFunc("/ftp", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "ftp://example.com/x", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	w := newWS(t, `{}`, true)
	if res, err := fetch(w, srv.URL+"/r/3"); err != nil || res.Content != "arrived" {
		t.Errorf("3 redirects: %v %q", err, res.Content)
	}
	if _, err := fetch(w, srv.URL+"/r/4"); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("4 redirects: %v", err)
	}
	if _, err := fetch(w, srv.URL+"/ftp"); err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Errorf("ftp redirect: %v", err)
	}
}

func TestFetchSSRFGuard(t *testing.T) {
	// A live loopback server must be unreachable without allowPrivate.
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	w := newWS(t, `{}`, false)
	for _, u := range []string{
		srv.URL,
		"http://127.0.0.1:1/",
		"http://10.1.2.3/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/",
		"http://[::ffff:127.0.0.1]/",
		"http://0.0.0.0/",
		"http://100.64.0.1/",
		"http://localhost/",
		"ftp://example.com/",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"",
	} {
		if _, err := fetch(w, u); err == nil {
			t.Errorf("%q: expected rejection", u)
		}
	}
	if hit {
		t.Error("request reached loopback server")
	}
}

func TestFetchGuardBlocksRedirectToLoopback(t *testing.T) {
	// Exercises the dialer-level check (as used on redirects) without the
	// pre-check: connect directly via the guarded client.
	hit := false
	inner := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { hit = true }))
	defer inner.Close()
	w := newWS(t, `{}`, false)
	resp, err := w.fetch.Get(inner.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("dial to loopback succeeded")
	}
	if hit {
		t.Error("server was hit")
	}
}

func TestBlockedIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.0.0.1": true, "192.168.0.5": true, "172.31.1.1": true,
		"169.254.1.1": true, "fe80::1": true, "fd00::1": true, "0.0.0.0": true, "224.0.0.1": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false,
	} {
		if got := blockedIP(net.ParseIP(ip)); got != want {
			t.Errorf("blockedIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestFetchStrictArgs(t *testing.T) {
	w := newWS(t, `{}`, true)
	h := w.fetchTool().Handler
	for _, bad := range []string{`{}`, `{"url":"http://x","extra":1}`, `[]`} {
		if _, err := h(context.Background(), json.RawMessage(bad)); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}
