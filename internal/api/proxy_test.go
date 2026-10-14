package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/doctor"
)

func mustNets(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestResolveClient(t *testing.T) {
	proxies := mustNets(t, "127.0.0.0/8", "10.0.0.0/8")
	cases := []struct {
		name       string
		peer       string
		nets       []*net.IPNet
		hdr        map[string]string
		wantIP     string
		wantScheme string
		wantHost   string
		wantIgn    bool
	}{
		{name: "no proxy, no headers", peer: "192.168.1.9:5000", wantIP: "192.168.1.9", wantScheme: "http", wantHost: "bosun.local:7777"},
		{name: "behind_proxy off ignores headers", peer: "127.0.0.1:5000",
			hdr:    map[string]string{"X-Forwarded-For": "1.2.3.4", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "evil.example"},
			wantIP: "127.0.0.1", wantScheme: "http", wantHost: "bosun.local:7777", wantIgn: true},
		{name: "untrusted peer ignored", peer: "192.168.1.9:5000", nets: proxies,
			hdr:    map[string]string{"X-Forwarded-For": "1.2.3.4", "X-Forwarded-Proto": "https"},
			wantIP: "192.168.1.9", wantScheme: "http", wantHost: "bosun.local:7777", wantIgn: true},
		{name: "trusted proxy", peer: "127.0.0.1:5000", nets: proxies,
			hdr:    map[string]string{"X-Forwarded-For": "100.80.1.2", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "box.tail1234.ts.net"},
			wantIP: "100.80.1.2", wantScheme: "https", wantHost: "box.tail1234.ts.net"},
		{name: "spoofed left-most hop skipped", peer: "127.0.0.1:5000", nets: proxies,
			hdr:    map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.7, 10.1.1.1", "X-Forwarded-Proto": "https"},
			wantIP: "203.0.113.7", wantScheme: "https", wantHost: "bosun.local:7777"},
		{name: "bad forwarded host ignored", peer: "127.0.0.1:5000", nets: proxies,
			hdr:    map[string]string{"X-Forwarded-Host": "evil.example/path"},
			wantIP: "127.0.0.1", wantScheme: "http", wantHost: "bosun.local:7777"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://bosun.local:7777/v1/health", nil)
			r.RemoteAddr = tc.peer
			for k, v := range tc.hdr {
				r.Header.Set(k, v)
			}
			c, ign := resolveClient(r, tc.nets)
			if c.IP != tc.wantIP || c.Scheme != tc.wantScheme || c.Host != tc.wantHost || ign != tc.wantIgn {
				t.Fatalf("got %+v ignored=%v; want ip=%s scheme=%s host=%s ignored=%v", c, ign, tc.wantIP, tc.wantScheme, tc.wantHost, tc.wantIgn)
			}
		})
	}
}

func TestCallbackURLBehindProxy(t *testing.T) {
	s := &Server{trustedProxies: mustNets(t, "127.0.0.0/8"), proxyStats: &ProxyStats{}, log: slog.New(slog.DiscardHandler)}
	var got string
	h := s.clientInfo(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = callbackURL(r) }))

	r := httptest.NewRequest("GET", "http://127.0.0.1:7777/x", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "bosun.example.com")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "https://bosun.example.com/oauth/callback" {
		t.Fatalf("callback = %s", got)
	}

	r = httptest.NewRequest("GET", "http://192.168.1.5:7777/x", nil)
	r.RemoteAddr = "192.168.1.20:1234" // not a trusted proxy
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "bosun.example.com")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "http://192.168.1.5:7777/oauth/callback" {
		t.Fatalf("untrusted callback = %s", got)
	}
	if n, ip := s.ProxyIgnored(); n != 1 || ip != "192.168.1.20" {
		t.Fatalf("ignored = %d %s", n, ip)
	}
}

func TestDoctorEndpoint(t *testing.T) {
	e := setup(t)
	tok := e.login(t)
	if resp := e.do(t, "GET", "/v1/doctor", tok, ""); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("no doctor: status %d", resp.StatusCode)
	}

	s, err := New(Options{Auth: e.auth, Bus: e.bus, Version: "v9", Doctor: func(context.Context) []doctor.Check {
		return []doctor.Check{{Name: "a", Status: doctor.OK}, {Name: "b", Status: doctor.Warn, Detail: "hmm"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e.srv = srv
	resp := e.do(t, "GET", "/v1/doctor", tok, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var dr DoctorResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		t.Fatal(err)
	}
	if dr.Version != "v9" || dr.Status != doctor.Warn || len(dr.Checks) != 2 {
		t.Fatalf("resp = %+v", dr)
	}

	// Members are refused.
	if _, err := e.auth.CreateUser(context.Background(), "kid", "longenough", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	resp = e.do(t, "POST", "/v1/login", "", `{"username":"kid","password":"longenough"}`)
	var lr LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(t, "GET", "/v1/doctor", lr.Token, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member: status %d", resp.StatusCode)
	}
}
