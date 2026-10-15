package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errBlockedAddr is returned for destinations the SSRF guard refuses.
var errBlockedAddr = errors.New("address is private, loopback, or otherwise not publicly routable")

// blockedIP reports whether ip must not be contacted (same rules as the
// websearch fetch guard).
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

// checkHost resolves host and refuses it if any address is blocked. It is an
// early, friendlier check; the proxy's dialer re-checks the address it
// actually connects to.
func checkHost(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return fmt.Errorf("refusing to open %s: %w", host, errBlockedAddr)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return fmt.Errorf("refusing to open %s: %w", host, errBlockedAddr)
		}
	}
	return nil
}

// guardProxy is a local HTTP proxy that Chromium is forced to use for every
// request (--proxy-server with loopback not bypassed). Its dialer refuses
// private, loopback and link-local destinations unless allowPrivate is set,
// so a page cannot make the browser reach the home network or the daemon
// (redirects, subresources, form posts and DNS rebinding included: the
// check runs on the address actually dialed).
type guardProxy struct {
	ln           net.Listener
	srv          *http.Server
	allowPrivate bool
	dialer       *net.Dialer
	transport    *http.Transport

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newGuardProxy(allowPrivate bool) (*guardProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("browser proxy: %w", err)
	}
	p := &guardProxy{ln: ln, allowPrivate: allowPrivate, conns: map[net.Conn]struct{}{}}
	p.dialer = &net.Dialer{
		Timeout: 15 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if p.allowPrivate {
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
	p.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           p.dialer.DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 15 * time.Second,
		ConnState: func(c net.Conn, s http.ConnState) {
			p.mu.Lock()
			defer p.mu.Unlock()
			switch s {
			case http.StateNew:
				p.conns[c] = struct{}{}
			case http.StateClosed, http.StateHijacked:
				delete(p.conns, c)
			}
		}}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// URL is the proxy's address for --proxy-server.
func (p *guardProxy) URL() string { return "http://" + p.ln.Addr().String() }

// Close stops the proxy and every tunnel.
func (p *guardProxy) Close() error {
	err := p.srv.Close()
	p.closeTunnels()
	p.transport.CloseIdleConnections()
	return err
}

// hopHeaders are removed when forwarding plain HTTP requests.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *guardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "bosun browser proxy: only absolute http:// requests", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		// Drop the connection rather than answer: the browser then shows a
		// network error page (as for a refused CONNECT), not a page of ours
		// that would look like the site.
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, herr := hj.Hijack(); herr == nil {
				c.Close()
				return
			}
		}
		http.Error(w, "bosun browser proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *guardProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	dst, err := p.dialer.DialContext(ctx, "tcp", host)
	cancel()
	if err != nil {
		http.Error(w, "bosun browser proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		dst.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	src, buf, err := hj.Hijack()
	if err != nil {
		dst.Close()
		return
	}
	p.track(src, true)
	p.track(dst, true)
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	done := make(chan struct{}, 2)
	go func() {
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			_, _ = dst.Write(b)
		}
		_, _ = io.Copy(dst, src)
		closeWrite(dst)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(src, dst)
		closeWrite(src)
		done <- struct{}{}
	}()
	go func() {
		<-done
		<-done
		src.Close()
		dst.Close()
		p.track(src, false)
		p.track(dst, false)
	}()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// track records hijacked tunnel connections so Close can end them.
func (p *guardProxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
}

// closeTunnels closes every tracked connection (used on session stop).
func (p *guardProxy) closeTunnels() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
}
