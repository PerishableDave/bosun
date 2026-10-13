package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"strconv"
	"strings"

	"github.com/dperedo/bosun/internal/integration"
)

// Credential kinds.
const (
	KindPassword    = "password"
	KindGoogleOAuth = "oauth2:google"
)

// Default Gmail endpoints for oauth2:google credentials.
const (
	gmailIMAPHost = "imap.gmail.com"
	gmailSMTPHost = "smtp.gmail.com"
	// gmailScope is the only OAuth scope Google accepts for IMAP/SMTP
	// XOAUTH2 (the OAuth manager's "imap" bundle).
	gmailScope = "https://mail.google.com/"
)

// errNoAccount is returned (wrapped) when the user has linked no account.
var errNoAccount = errors.New("no email account linked (not configured). Run `bosun connect email` to add one with an app password " +
	"(or store it directly with `bosun credentials set email password --field host=imap.example.com --field user=you@example.com`, " +
	"secret = the app password), or link Google with `bosun connect google --scopes imap`")

// account is a resolved mailbox: where to connect and how to log in. It is
// built per call from the calling user's credential.
type account struct {
	kind     string
	email    string // the From address
	name     string // optional display name
	user     string // login name
	secret   string // password or access token
	oauth    bool
	imapHost string
	imapPort int
	imapTLS  bool // implicit TLS; false = STARTTLS
	smtp     []smtpEndpoint
	saveSent bool // APPEND sent mail to the Sent folder
}

func (a *account) gmail() bool {
	return strings.EqualFold(a.imapHost, gmailIMAPHost) || a.kind == KindGoogleOAuth
}

// resolveAccount loads the calling user's credential: oauth2:google first,
// then password.
func (e *Integration) resolveAccount(ctx context.Context) (*account, error) {
	if e.creds == nil {
		return nil, fmt.Errorf("email: %w", errNoAccount)
	}
	cred, gerr := e.creds.Get(ctx, KindGoogleOAuth)
	if gerr == nil {
		return e.googleAccount(cred)
	}
	pcred, perr := e.creds.Get(ctx, KindPassword)
	if perr == nil {
		return passwordAccount(pcred)
	}
	if !errors.Is(gerr, integration.ErrNoCredential) {
		return nil, fmt.Errorf("email: google credential: %w", gerr)
	}
	if !errors.Is(perr, integration.ErrNoCredential) {
		return nil, fmt.Errorf("email: password credential: %w", perr)
	}
	return nil, fmt.Errorf("email: %w", errNoAccount)
}

func (e *Integration) googleAccount(c integration.Credential) (*account, error) {
	addr := strings.TrimSpace(c.Fields["email"])
	if c.Secret == "" || addr == "" || !validAddr(addr) {
		return nil, errors.New("email: the linked Google account is missing its access token or address; run `bosun connect google --scopes imap` again")
	}
	if sc := c.Fields["scopes"]; sc != "" && !strings.Contains(sc, gmailScope) {
		return nil, errors.New("email: the linked Google account was not granted mail access (scope " + gmailScope +
			"); run `bosun connect google --scopes imap` (add other bundles, e.g. --scopes imap,calendar)")
	}
	host, port := splitHostPort(e.googleIMAP, 993)
	shost, sport := splitHostPort(e.googleSMTP, 465)
	a := &account{
		kind: KindGoogleOAuth, email: addr, user: addr, secret: c.Secret, oauth: true,
		imapHost: host, imapPort: port, imapTLS: true,
		smtp:     []smtpEndpoint{{Host: shost, Port: sport, Implicit: true}},
		saveSent: false, // Gmail files submitted mail into Sent itself
	}
	if sport == 465 {
		a.smtp = append(a.smtp, smtpEndpoint{Host: shost, Port: 587})
	}
	return a, nil
}

func splitHostPort(hp string, def int) (string, int) {
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		return hp, def
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return h, def
	}
	return h, n
}

func validAddr(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && !strings.ContainsAny(s, "<>\r\n ")
}

func port(fields map[string]string, key string, def int) (int, error) {
	s := strings.TrimSpace(fields[key])
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 65535 {
		return 0, fmt.Errorf("email: credential field %s=%q is not a port number", key, s)
	}
	return n, nil
}

func safeField(s string) bool { return !strings.ContainsAny(s, "\r\n\x00") }

// passwordAccount builds an account from a "password" credential:
//
//	Secret: the (app) password
//	Fields: host (IMAP host, required), user (login, required),
//	        email (From address; default user), name (display name),
//	        imap_port (default 993; 143 uses STARTTLS),
//	        smtp_host (default host with a leading "imap." replaced by "smtp."),
//	        smtp_port (default 465 with 587+STARTTLS fallback; 465 = implicit
//	        TLS, anything else STARTTLS), save_sent (true/false; default true,
//	        false for Gmail, which files sent mail itself).
func passwordAccount(c integration.Credential) (*account, error) {
	f := c.Fields
	host := strings.TrimSpace(f["host"])
	user := strings.TrimSpace(f["user"])
	if host == "" || user == "" {
		return nil, errors.New("email: the password credential needs --field host=<imap host> and --field user=<login>; " +
			"e.g. `bosun credentials set email password --field host=imap.fastmail.com --field user=me@fastmail.com`")
	}
	if c.Secret == "" {
		return nil, errors.New("email: the password credential has an empty secret")
	}
	for _, k := range []string{"host", "user", "smtp_host", "email", "name"} {
		if !safeField(f[k]) {
			return nil, fmt.Errorf("email: credential field %s contains a line break", k)
		}
	}
	addr := strings.TrimSpace(f["email"])
	if addr == "" {
		addr = user
	}
	if !validAddr(addr) {
		return nil, fmt.Errorf("email: %q is not an email address; set --field email=<your address>", addr)
	}
	ip, err := port(f, "imap_port", 993)
	if err != nil {
		return nil, err
	}
	shost := strings.TrimSpace(f["smtp_host"])
	if shost == "" {
		shost = host
		if strings.HasPrefix(strings.ToLower(host), "imap.") {
			shost = "smtp." + host[len("imap."):]
		}
	}
	a := &account{
		kind: KindPassword, email: addr, name: strings.TrimSpace(f["name"]), user: user, secret: c.Secret,
		imapHost: host, imapPort: ip, imapTLS: ip != 143,
	}
	if strings.TrimSpace(f["smtp_port"]) == "" {
		a.smtp = []smtpEndpoint{{Host: shost, Port: 465, Implicit: true}, {Host: shost, Port: 587}}
	} else {
		sp, err := port(f, "smtp_port", 465)
		if err != nil {
			return nil, err
		}
		a.smtp = []smtpEndpoint{{Host: shost, Port: sp, Implicit: sp == 465}}
	}
	a.saveSent = !a.gmail()
	if v := strings.ToLower(strings.TrimSpace(f["save_sent"])); v != "" {
		a.saveSent = v == "true" || v == "yes" || v == "1"
	}
	return a, nil
}

// tlsFor returns the TLS config for host.
func (e *Integration) tlsFor(host string) *tls.Config {
	var cfg *tls.Config
	if e.tlsConfig != nil {
		cfg = e.tlsConfig.Clone()
	} else {
		cfg = &tls.Config{}
	}
	cfg.ServerName = host
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return cfg
}

// openIMAP connects, logs in and returns a ready connection. The caller
// must logout().
func (e *Integration) openIMAP(ctx context.Context, a *account) (*imapConn, error) {
	addr := net.JoinHostPort(a.imapHost, strconv.Itoa(a.imapPort))
	dctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	raw, err := e.dial(dctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("imap: connect %s: %w", addr, err)
	}
	conn := raw
	if a.imapTLS {
		tc := tls.Client(raw, e.tlsFor(a.imapHost))
		if err := tc.HandshakeContext(dctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("imap: tls %s: %w", addr, err)
		}
		conn = tc
	}
	c := newIMAPConn(conn, e.timeout)
	c.stop = context.AfterFunc(ctx, func() { raw.Close() })
	ok := false
	defer func() {
		if !ok {
			c.stop()
			raw.Close()
		}
	}()
	if err := c.greet(); err != nil {
		return nil, err
	}
	if err := c.capability(); err != nil {
		return nil, err
	}
	if !a.imapTLS {
		if err := c.startTLS(dctx, e.tlsFor(a.imapHost)); err != nil {
			return nil, err
		}
	}
	if a.oauth {
		err = c.authenticateXOAUTH2(a.user, a.secret)
	} else {
		err = c.login(a.user, a.secret)
	}
	if err != nil {
		var ie *IMAPError
		if errors.As(err, &ie) {
			return nil, &loginRejectedError{host: a.imapHost, user: a.user, text: ie.Text}
		}
		return nil, err
	}
	if err := c.capability(); err != nil {
		return nil, err
	}
	ok = true
	return c, nil
}
