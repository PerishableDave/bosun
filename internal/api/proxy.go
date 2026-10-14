package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Client describes who sent a request and how they reached the daemon,
// after applying daemon.behind_proxy: forwarded headers are believed only
// when the TCP peer is a trusted proxy.
type Client struct {
	// IP is the client's address: the TCP peer, or with a trusted proxy the
	// right-most untrusted address in X-Forwarded-For.
	IP string
	// Scheme is "https" or "http" as the client used it.
	Scheme string
	// Host is the host (and port) the client addressed.
	Host string
	// Proxied is true when forwarded headers from a trusted proxy were used.
	Proxied bool
}

type clientKey struct{}

// ClientFromContext returns the request's Client (set by the proxy
// middleware in Handler).
func ClientFromContext(ctx context.Context) (Client, bool) {
	c, ok := ctx.Value(clientKey{}).(Client)
	return c, ok
}

// ProxyStats counts requests that carried X-Forwarded-* headers bosund did
// not believe (behind_proxy off, or a peer outside trusted_proxies). bosun
// doctor uses it to spot a proxy that is missing from trusted_proxies.
type ProxyStats struct {
	ignored atomic.Int64
	mu      sync.Mutex
	lastIP  string
}

// Ignored returns the count and the last peer that sent ignored headers.
func (p *ProxyStats) Ignored() (int64, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ignored.Load(), p.lastIP
}

func (p *ProxyStats) record(ip string) {
	p.ignored.Add(1)
	p.mu.Lock()
	p.lastIP = ip
	p.mu.Unlock()
}

func peerIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func trusted(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// lastValue returns the last comma-separated value of a header (the one the
// nearest proxy appended or set).
func lastValue(h http.Header, name string) string {
	vals := h.Values(name)
	if len(vals) == 0 {
		return ""
	}
	parts := strings.Split(vals[len(vals)-1], ",")
	return strings.TrimSpace(parts[len(parts)-1])
}

func hasForwarded(h http.Header) bool {
	return h.Get("X-Forwarded-For") != "" || h.Get("X-Forwarded-Proto") != "" || h.Get("X-Forwarded-Host") != ""
}

// resolveClient computes the Client for r given the trusted proxy networks
// (nil: never trust forwarded headers).
func resolveClient(r *http.Request, nets []*net.IPNet) (Client, bool) {
	c := Client{Scheme: "http", Host: r.Host}
	if r.TLS != nil {
		c.Scheme = "https"
	}
	peer := peerIP(r)
	if peer != nil {
		c.IP = peer.String()
	} else {
		c.IP = r.RemoteAddr
	}
	if !hasForwarded(r.Header) {
		return c, false
	}
	if !trusted(peer, nets) {
		return c, true // headers present but not believed
	}
	c.Proxied = true
	// X-Forwarded-For: walk right to left past trusted proxies; the first
	// untrusted hop is the client. Anything further left is client-supplied.
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil {
			break
		}
		c.IP = ip.String()
		if !trusted(ip, nets) {
			break
		}
	}
	if p := strings.ToLower(lastValue(r.Header, "X-Forwarded-Proto")); p == "http" || p == "https" {
		c.Scheme = p
	}
	if h := lastValue(r.Header, "X-Forwarded-Host"); h != "" && validHost(h) {
		c.Host = h
	}
	return c, false
}

// validHost accepts host or host:port with no path, userinfo, or spaces.
func validHost(h string) bool {
	if len(h) > 255 || strings.ContainsAny(h, "/\\@ ?#\t\r\n") {
		return false
	}
	return true
}

// clientInfo is the middleware that resolves the Client for every request.
func (s *Server) clientInfo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ignored := resolveClient(r, s.trustedProxies)
		if ignored && s.proxyStats != nil {
			s.proxyStats.record(c.IP)
			s.log.DebugContext(r.Context(), "ignoring X-Forwarded-* headers from an untrusted peer", "peer", c.IP, "behind_proxy", s.trustedProxies != nil)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, c)))
	})
}
