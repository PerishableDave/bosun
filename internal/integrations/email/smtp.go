package email

// A minimal SMTP submission client (RFC 5321 / 6409): EHLO, STARTTLS,
// AUTH PLAIN / LOGIN / XOAUTH2, MAIL, RCPT, DATA. Credentials are only ever
// sent over TLS (implicit on 465, STARTTLS otherwise).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	maxSMTPLine  = 4096
	maxSMTPLines = 200
)

// SMTPError is an unexpected SMTP reply.
type SMTPError struct {
	Command string
	Code    int
	Text    string
}

func (e *SMTPError) Error() string {
	return fmt.Sprintf("smtp %s failed: %d %s", e.Command, e.Code, e.Text)
}

type smtpConn struct {
	conn    net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	ext     map[string]string
	timeout time.Duration
}

func newSMTPConn(conn net.Conn, timeout time.Duration) *smtpConn {
	return &smtpConn{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn), timeout: timeout}
}

func (c *smtpConn) deadline() { _ = c.conn.SetDeadline(time.Now().Add(c.timeout)) }

// reply reads a (possibly multi-line) reply.
func (c *smtpConn) reply() (int, []string, error) {
	var lines []string
	code := 0
	for {
		line, err := c.r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return 0, nil, errors.New("smtp: reply line too long")
			}
			return 0, nil, err
		}
		if len(line) > maxSMTPLine {
			return 0, nil, errors.New("smtp: reply line too long")
		}
		s := strings.TrimRight(string(line), "\r\n")
		if len(s) < 3 {
			return 0, nil, fmt.Errorf("smtp: malformed reply %q", s)
		}
		n, err := strconv.Atoi(s[:3])
		if err != nil {
			return 0, nil, fmt.Errorf("smtp: malformed reply %q", s)
		}
		if code == 0 {
			code = n
		}
		text := ""
		if len(s) > 4 {
			text = s[4:]
		}
		lines = append(lines, text)
		if len(lines) > maxSMTPLines {
			return 0, nil, errors.New("smtp: reply too long")
		}
		if len(s) == 3 || s[3] != '-' {
			return code, lines, nil
		}
	}
}

// cmd sends line and expects a reply with code
// expect. name labels errors; the line itself is never included (AUTH
// lines carry credentials).
func (c *smtpConn) cmd(name, line string, expect ...int) (int, string, error) {
	c.deadline()
	c.w.WriteString(line + "\r\n")
	if err := c.w.Flush(); err != nil {
		return 0, "", fmt.Errorf("smtp %s: %w", name, err)
	}
	code, lines, err := c.reply()
	if err != nil {
		return 0, "", fmt.Errorf("smtp %s: %w", name, err)
	}
	text := strings.Join(lines, " ")
	for _, e := range expect {
		if code == e {
			return code, text, nil
		}
	}
	return code, text, &SMTPError{Command: name, Code: code, Text: text}
}

// hello sends EHLO and records the advertised extensions.
func (c *smtpConn) hello(name string) error {
	c.deadline()
	c.w.WriteString("EHLO " + name + "\r\n")
	if err := c.w.Flush(); err != nil {
		return fmt.Errorf("smtp EHLO: %w", err)
	}
	code, lines, err := c.reply()
	if err != nil {
		return fmt.Errorf("smtp EHLO: %w", err)
	}
	if code != 250 {
		return &SMTPError{Command: "EHLO", Code: code, Text: strings.Join(lines, " ")}
	}
	c.ext = map[string]string{}
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(l, " ")
		c.ext[strings.ToUpper(k)] = v
	}
	return nil
}

func (c *smtpConn) auth(acct *account) error {
	if acct.oauth {
		code, text, err := c.cmd("AUTH", "AUTH XOAUTH2 "+xoauth2(acct.user, acct.secret), 235, 334)
		if err != nil {
			return err
		}
		if code == 334 {
			// Error challenge; an empty response yields the final error.
			code, text2, _ := c.cmd("AUTH", "", 235)
			if code != 235 {
				return &SMTPError{Command: "AUTH", Code: code, Text: decodeChallenge(text) + " " + text2}
			}
		}
		return nil
	}
	mechs := " " + strings.ToUpper(c.ext["AUTH"]) + " "
	switch {
	case strings.Contains(mechs, " PLAIN "):
		ir := base64.StdEncoding.EncodeToString([]byte("\x00" + acct.user + "\x00" + acct.secret))
		_, _, err := c.cmd("AUTH", "AUTH PLAIN "+ir, 235)
		return err
	case strings.Contains(mechs, " LOGIN "):
		if _, _, err := c.cmd("AUTH", "AUTH LOGIN", 334); err != nil {
			return err
		}
		if _, _, err := c.cmd("AUTH", base64.StdEncoding.EncodeToString([]byte(acct.user)), 334); err != nil {
			return err
		}
		_, _, err := c.cmd("AUTH", base64.StdEncoding.EncodeToString([]byte(acct.secret)), 235)
		return err
	default:
		return errors.New("smtp: server offers no supported AUTH mechanism (PLAIN or LOGIN)")
	}
}

func decodeChallenge(s string) string {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return string(b)
}

// dotStuff normalises line endings to CRLF and escapes leading dots.
func dotStuff(msg []byte) []byte {
	msg = bytes.ReplaceAll(msg, []byte("\r\n"), []byte("\n"))
	lines := bytes.Split(msg, []byte("\n"))
	var b bytes.Buffer
	for i, l := range lines {
		if i == len(lines)-1 && len(l) == 0 {
			break
		}
		if len(l) > 0 && l[0] == '.' {
			b.WriteByte('.')
		}
		b.Write(bytes.TrimRight(l, "\r"))
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// smtpEndpoint is one way to reach the submission server.
type smtpEndpoint struct {
	Host     string
	Port     int
	Implicit bool // implicit TLS (465); otherwise STARTTLS is required
}

// sendMail submits msg from -> rcpts using the account's SMTP endpoints in
// order, falling back to the next one only when connecting fails.
func (e *Integration) sendMail(ctx context.Context, acct *account, from string, rcpts []string, msg []byte) (string, error) {
	var errs []error
	for i, ep := range acct.smtp {
		c, err := e.dialSMTP(ctx, ep)
		if err != nil {
			errs = append(errs, err)
			if i < len(acct.smtp)-1 {
				continue
			}
			return "", errors.Join(errs...)
		}
		id, err := c.submit(ctx, e, ep, acct, from, rcpts, msg)
		c.conn.Close()
		return id, err
	}
	return "", errors.New("smtp: no server configured")
}

func (e *Integration) dialSMTP(ctx context.Context, ep smtpEndpoint) (*smtpConn, error) {
	addr := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	dctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	raw, err := e.dial(dctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("smtp: connect %s: %w", addr, err)
	}
	conn := raw
	if ep.Implicit {
		tc := tls.Client(raw, e.tlsFor(ep.Host))
		if err := tc.HandshakeContext(dctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("smtp: tls %s: %w", addr, err)
		}
		conn = tc
	}
	return newSMTPConn(conn, e.timeout), nil
}

// login reads the greeting, says EHLO, upgrades to TLS when the endpoint
// uses STARTTLS, and authenticates.
func (c *smtpConn) login(ctx context.Context, e *Integration, ep smtpEndpoint, acct *account) error {
	c.deadline()
	code, lines, err := c.reply()
	if err != nil {
		return fmt.Errorf("smtp greeting: %w", err)
	}
	if code != 220 {
		return &SMTPError{Command: "greeting", Code: code, Text: strings.Join(lines, " ")}
	}
	helo := e.heloName()
	if err := c.hello(helo); err != nil {
		return err
	}
	if !ep.Implicit {
		if _, ok := c.ext["STARTTLS"]; !ok {
			return errors.New("smtp: server does not offer STARTTLS; refusing to send credentials without TLS")
		}
		if _, _, err := c.cmd("STARTTLS", "STARTTLS", 220); err != nil {
			return err
		}
		tc := tls.Client(c.conn, e.tlsFor(ep.Host))
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("smtp: starttls: %w", err)
		}
		c.conn, c.r, c.w = tc, bufio.NewReader(tc), bufio.NewWriter(tc)
		if err := c.hello(helo); err != nil {
			return err
		}
	}
	return c.auth(acct)
}

// checkSMTP connects to the account's submission server (falling back to
// the next endpoint only when connecting fails, like sendMail), logs in and
// quits without sending anything.
func (e *Integration) checkSMTP(ctx context.Context, acct *account) error {
	var errs []error
	for i, ep := range acct.smtp {
		c, err := e.dialSMTP(ctx, ep)
		if err != nil {
			errs = append(errs, err)
			if i < len(acct.smtp)-1 {
				continue
			}
			return errors.Join(errs...)
		}
		raw := c.conn
		stop := context.AfterFunc(ctx, func() { raw.Close() })
		err = c.login(ctx, e, ep, acct)
		if err == nil {
			_, _, _ = c.cmd("QUIT", "QUIT", 221)
		}
		stop()
		c.conn.Close()
		return err
	}
	return errors.New("smtp: no server configured")
}

func (c *smtpConn) submit(ctx context.Context, e *Integration, ep smtpEndpoint, acct *account, from string, rcpts []string, msg []byte) (string, error) {
	raw := c.conn
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	if err := c.login(ctx, e, ep, acct); err != nil {
		return "", err
	}
	if _, _, err := c.cmd("MAIL", "MAIL FROM:<"+from+">", 250); err != nil {
		return "", err
	}
	for _, r := range rcpts {
		if _, _, err := c.cmd("RCPT", "RCPT TO:<"+r+">", 250, 251); err != nil {
			_, _, _ = c.cmd("RSET", "RSET", 250)
			return "", fmt.Errorf("recipient %s rejected: %w", r, err)
		}
	}
	if _, _, err := c.cmd("DATA", "DATA", 354); err != nil {
		return "", err
	}
	c.deadline()
	c.w.Write(dotStuff(msg))
	_, text, err := c.cmd("DATA", ".", 250)
	if err != nil {
		return "", err
	}
	_, _, _ = c.cmd("QUIT", "QUIT", 221)
	return text, nil
}
