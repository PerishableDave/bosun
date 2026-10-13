package email

// A fake SMTP submission server: EHLO, STARTTLS, AUTH PLAIN / LOGIN /
// XOAUTH2, MAIL, RCPT, DATA, RSET, QUIT.

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"net"
	"strings"
	"sync"
	"testing"
)

type sentMail struct {
	authUser string
	from     string
	rcpts    []string
	data     string
}

type fakeSMTP struct {
	ln       net.Listener
	tlsCfg   *tls.Config
	implicit bool
	mechs    string // advertised AUTH mechanisms

	mu        sync.Mutex
	passwords map[string]string
	tokens    map[string]string
	sent      []sentMail
}

func newFakeSMTP(t *testing.T, tlsCfg *tls.Config, implicit bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln, tlsCfg: tlsCfg, implicit: implicit, mechs: "PLAIN LOGIN XOAUTH2",
		passwords: map[string]string{}, tokens: map[string]string{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.session(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeSMTP) addr() string { return s.ln.Addr().String() }

func (s *fakeSMTP) messages() []sentMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentMail(nil), s.sent...)
}

func (s *fakeSMTP) session(c net.Conn) {
	defer c.Close()
	conn := c
	isTLS := false
	if s.implicit {
		tc := tls.Server(c, s.tlsCfg)
		if tc.Handshake() != nil {
			return
		}
		conn, isTLS = tc, true
	}
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	say := func(l string) { w.WriteString(l + "\r\n"); w.Flush() }
	read := func() (string, bool) {
		l, err := r.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err == nil
	}
	say("220 fake.smtp ESMTP ready")
	authUser := ""
	var cur *sentMail
	for {
		l, ok := read()
		if !ok {
			return
		}
		verb, rest, _ := strings.Cut(l, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			say("250-fake.smtp hello " + rest)
			if !isTLS {
				say("250-STARTTLS")
			}
			say("250-AUTH " + s.mechs)
			say("250 8BITMIME")
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, s.tlsCfg)
			if tc.Handshake() != nil {
				return
			}
			conn, isTLS = tc, true
			r, w = bufio.NewReader(conn), bufio.NewWriter(conn)
		case "AUTH":
			if !isTLS {
				say("538 encryption required")
				continue
			}
			mech, arg, _ := strings.Cut(rest, " ")
			switch strings.ToUpper(mech) {
			case "PLAIN":
				raw, _ := base64.StdEncoding.DecodeString(arg)
				p := strings.Split(string(raw), "\x00")
				s.mu.Lock()
				good := len(p) == 3 && s.passwords[p[1]] != "" && s.passwords[p[1]] == p[2]
				s.mu.Unlock()
				if good {
					authUser = p[1]
					say("235 2.7.0 accepted")
				} else {
					say("535 5.7.8 bad credentials")
				}
			case "LOGIN":
				say("334 VXNlcm5hbWU6")
				u, _ := read()
				say("334 UGFzc3dvcmQ6")
				pw, _ := read()
				ub, _ := base64.StdEncoding.DecodeString(u)
				pb, _ := base64.StdEncoding.DecodeString(pw)
				s.mu.Lock()
				good := s.passwords[string(ub)] != "" && s.passwords[string(ub)] == string(pb)
				s.mu.Unlock()
				if good {
					authUser = string(ub)
					say("235 2.7.0 accepted")
				} else {
					say("535 5.7.8 bad credentials")
				}
			case "XOAUTH2":
				raw, _ := base64.StdEncoding.DecodeString(arg)
				p := strings.Split(string(raw), "\x01")
				user := strings.TrimPrefix(p[0], "user=")
				tok := ""
				if len(p) > 1 {
					tok = strings.TrimPrefix(p[1], "auth=Bearer ")
				}
				s.mu.Lock()
				good := s.tokens[user] != "" && s.tokens[user] == tok
				s.mu.Unlock()
				if good {
					authUser = user
					say("235 2.7.0 accepted")
				} else {
					say("334 " + base64.StdEncoding.EncodeToString([]byte(`{"status":"401"}`)))
					read()
					say("535 5.7.8 Username and Password not accepted")
				}
			default:
				say("504 unrecognised mechanism")
			}
		case "MAIL":
			if authUser == "" {
				say("530 authentication required")
				continue
			}
			cur = &sentMail{authUser: authUser, from: strings.Trim(strings.TrimPrefix(rest, "FROM:"), "<>")}
			say("250 ok")
		case "RCPT":
			addr := strings.Trim(strings.TrimPrefix(rest, "TO:"), "<>")
			if strings.HasPrefix(addr, "reject@") {
				say("550 5.1.1 no such user")
				continue
			}
			cur.rcpts = append(cur.rcpts, addr)
			say("250 ok")
		case "DATA":
			say("354 end with <CRLF>.<CRLF>")
			var b strings.Builder
			for {
				l, ok := read()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".") + "\n")
			}
			cur.data = b.String()
			s.mu.Lock()
			s.sent = append(s.sent, *cur)
			s.mu.Unlock()
			say("250 2.0.0 OK queued as 12345")
		case "RSET":
			cur = nil
			say("250 ok")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 unknown")
		}
	}
}
