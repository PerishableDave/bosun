package email

// A fake IMAP4rev1 server speaking just enough of the protocol for the
// client: CAPABILITY, STARTTLS, LOGIN, AUTHENTICATE XOAUTH2, LIST, SELECT,
// EXAMINE, (UID) SEARCH, (UID) FETCH with literals, APPEND (synchronising
// and LITERAL+), UID STORE, UID COPY, UID MOVE, (UID) EXPUNGE, CREATE,
// LOGOUT.

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCert returns a server TLS config and a client pool trusting it.
func testCert(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake mail"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames:    []string{"localhost", "imap.example.test", "smtp.example.test", "imap.gmail.com", "smtp.gmail.com"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

type fakeMsg struct {
	uid   uint32
	flags map[string]bool
	raw   []byte
	date  time.Time
}

type fakeBox struct {
	attrs    []string
	validity uint32
	next     uint32
	msgs     []*fakeMsg
}

type fakeAccount struct {
	password string
	token    string
	boxes    map[string]*fakeBox
}

type fakeIMAP struct {
	t        *testing.T
	ln       net.Listener
	tlsCfg   *tls.Config
	implicit bool
	caps     []string // optional extensions: MOVE, UIDPLUS, SASL-IR, LITERAL+

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	logins   []string // successful logins (user names), in order
	commands []string // command verbs received
}

func newFakeIMAP(t *testing.T, tlsCfg *tls.Config, implicit bool, caps ...string) *fakeIMAP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeIMAP{t: t, ln: ln, tlsCfg: tlsCfg, implicit: implicit, caps: caps, accounts: map[string]*fakeAccount{}}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeIMAP) addr() string { return s.ln.Addr().String() }

// account creates (or returns) a user with the standard folders.
func (s *fakeIMAP) account(user string) *fakeAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[user]; ok {
		return a
	}
	a := &fakeAccount{boxes: map[string]*fakeBox{
		"INBOX":        {validity: 1, next: 1},
		"Drafts":       {attrs: []string{`\Drafts`}, validity: 2, next: 1},
		"Sent Items":   {attrs: []string{`\Sent`}, validity: 3, next: 1},
		"Archive":      {attrs: []string{`\Archive`}, validity: 4, next: 1},
		"Entw&APw-rfe": {validity: 5, next: 1},
		"[Gmail]":      {attrs: []string{`\Noselect`, `\HasChildren`}, validity: 6, next: 1},
	}}
	s.accounts[user] = a
	return a
}

func (s *fakeIMAP) setPassword(user, pw string) {
	a := s.account(user)
	s.mu.Lock()
	a.password = pw
	s.mu.Unlock()
}
func (s *fakeIMAP) setToken(user, tok string) {
	a := s.account(user)
	s.mu.Lock()
	a.token = tok
	s.mu.Unlock()
}

// deliver adds a message to user's folder and returns its UID.
func (s *fakeIMAP) deliver(user, folder string, raw string, flags ...string) uint32 {
	a := s.account(user)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := a.boxes[folder]
	m := &fakeMsg{uid: b.next, flags: map[string]bool{}, raw: []byte(strings.ReplaceAll(raw, "\n", "\r\n")), date: time.Now()}
	for _, f := range flags {
		m.flags[f] = true
	}
	b.next++
	b.msgs = append(b.msgs, m)
	return m.uid
}

func (s *fakeIMAP) box(user, folder string) *fakeBox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[user].boxes[folder]
}

func (s *fakeIMAP) msgs(user, folder string) []*fakeMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fakeMsg(nil), s.accounts[user].boxes[folder].msgs...)
}

func (s *fakeIMAP) loginsSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logins...)
}

func (s *fakeIMAP) sawCommand(verb string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.commands {
		if c == verb {
			return true
		}
	}
	return false
}

func (s *fakeIMAP) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.session(c)
	}
}

type imapSession struct {
	s        *fakeIMAP
	conn     net.Conn
	r        *bufio.Reader
	w        *bufio.Writer
	tls      bool
	acct     *fakeAccount
	user     string
	sel      *fakeBox
	readOnly bool
}

func (s *fakeIMAP) session(c net.Conn) {
	defer c.Close()
	ss := &imapSession{s: s, conn: c}
	if s.implicit {
		tc := tls.Server(c, s.tlsCfg)
		if err := tc.Handshake(); err != nil {
			return
		}
		ss.conn, ss.tls = tc, true
	}
	ss.r, ss.w = bufio.NewReader(ss.conn), bufio.NewWriter(ss.conn)
	ss.line("* OK [CAPABILITY IMAP4rev1] fake IMAP ready (say \"hi\")")
	for {
		tag, args, err := ss.readCommand()
		if err != nil {
			return
		}
		if len(args) == 0 {
			ss.line(tag + " BAD empty command")
			continue
		}
		verb := strings.ToUpper(args[0])
		if verb == "UID" && len(args) > 1 {
			verb = "UID " + strings.ToUpper(args[1])
			args = args[1:]
		}
		s.mu.Lock()
		s.commands = append(s.commands, verb)
		s.mu.Unlock()
		if !ss.handle(tag, verb, args[1:]) {
			return
		}
	}
}

func (ss *imapSession) line(s string) {
	ss.w.WriteString(s + "\r\n")
	ss.w.Flush()
}

// readCommand tokenises one command line: atoms, quoted strings and
// literals become tokens; "(" and ")" are tokens of their own.
func (ss *imapSession) readCommand() (string, []string, error) {
	var toks []string
	for {
		c, err := ss.r.ReadByte()
		if err != nil {
			return "", nil, err
		}
		switch {
		case c == ' ':
		case c == '\r':
		case c == '\n':
			if len(toks) == 0 {
				continue
			}
			return toks[0], toks[1:], nil
		case c == '(' || c == ')':
			toks = append(toks, string(c))
		case c == '"':
			var b []byte
			for {
				c, err := ss.r.ReadByte()
				if err != nil {
					return "", nil, err
				}
				if c == '\\' {
					c, _ = ss.r.ReadByte()
				} else if c == '"' {
					break
				}
				b = append(b, c)
			}
			toks = append(toks, string(b))
		case c == '{':
			spec, err := ss.r.ReadString('}')
			if err != nil {
				return "", nil, err
			}
			spec = strings.TrimSuffix(spec, "}")
			plus := strings.HasSuffix(spec, "+")
			n, _ := strconv.Atoi(strings.TrimSuffix(spec, "+"))
			ss.r.ReadString('\n')
			if !plus {
				ss.line("+ Ready for literal data")
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(ss.r, buf); err != nil {
				return "", nil, err
			}
			toks = append(toks, string(buf))
		default:
			b := []byte{c}
			for {
				p, err := ss.r.Peek(1)
				if err != nil {
					return "", nil, err
				}
				if p[0] == ' ' || p[0] == '\r' || p[0] == '\n' || p[0] == '(' || p[0] == ')' {
					break
				}
				ss.r.ReadByte()
				b = append(b, p[0])
			}
			toks = append(toks, string(b))
		}
	}
}

func (ss *imapSession) has(c string) bool {
	for _, x := range ss.s.caps {
		if x == c {
			return true
		}
	}
	return false
}

func imapQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// handle runs one command; false ends the session.
func (ss *imapSession) handle(tag, verb string, args []string) bool {
	s := ss.s
	needAuth := func() bool {
		if ss.acct == nil {
			ss.line(tag + " NO not authenticated")
			return false
		}
		return true
	}
	needSel := func() bool {
		if !needAuth() {
			return false
		}
		if ss.sel == nil {
			ss.line(tag + " BAD no mailbox selected")
			return false
		}
		return true
	}
	switch verb {
	case "CAPABILITY":
		caps := "IMAP4rev1 AUTH=PLAIN AUTH=XOAUTH2 " + strings.Join(s.caps, " ")
		if !ss.tls {
			caps += " STARTTLS"
		}
		ss.line("* CAPABILITY " + strings.TrimSpace(caps))
		ss.line(tag + " OK CAPABILITY completed")
	case "STARTTLS":
		ss.line(tag + " OK begin TLS")
		tc := tls.Server(ss.conn, s.tlsCfg)
		if err := tc.Handshake(); err != nil {
			return false
		}
		ss.conn, ss.tls = tc, true
		ss.r, ss.w = bufio.NewReader(tc), bufio.NewWriter(tc)
	case "LOGIN":
		if !ss.tls {
			ss.line(tag + " NO [PRIVACYREQUIRED] TLS required")
			return true
		}
		s.mu.Lock()
		a, ok := s.accounts[args[0]]
		good := ok && a.password != "" && a.password == args[1]
		if good {
			s.logins = append(s.logins, args[0])
		}
		s.mu.Unlock()
		if !good {
			ss.line(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials")
			return true
		}
		ss.acct, ss.user = a, args[0]
		ss.line(tag + " OK [CAPABILITY IMAP4rev1 MOVE] LOGIN completed")
	case "AUTHENTICATE":
		if len(args) < 1 || !strings.EqualFold(args[0], "XOAUTH2") {
			ss.line(tag + " NO unsupported mechanism")
			return true
		}
		var ir string
		if len(args) > 1 {
			ir = args[1]
		} else {
			ss.line("+ ")
			l, err := ss.r.ReadString('\n')
			if err != nil {
				return false
			}
			ir = strings.TrimSpace(l)
		}
		raw, _ := base64.StdEncoding.DecodeString(ir)
		parts := strings.Split(string(raw), "\x01")
		user := strings.TrimPrefix(parts[0], "user=")
		tok := ""
		if len(parts) > 1 {
			tok = strings.TrimPrefix(parts[1], "auth=Bearer ")
		}
		s.mu.Lock()
		a, ok := s.accounts[user]
		good := ok && a.token != "" && a.token == tok
		if good {
			s.logins = append(s.logins, user)
		}
		s.mu.Unlock()
		if !good {
			ss.line("+ " + base64.StdEncoding.EncodeToString([]byte(`{"status":"401","schemes":"Bearer"}`)))
			if _, err := ss.r.ReadString('\n'); err != nil {
				return false
			}
			ss.line(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)")
			return true
		}
		ss.acct, ss.user = a, user
		ss.line(tag + " OK authenticated")
	case "LIST":
		if !needAuth() {
			return true
		}
		s.mu.Lock()
		names := make([]string, 0, len(ss.acct.boxes))
		for n := range ss.acct.boxes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			attrs := strings.Join(ss.acct.boxes[n].attrs, " ")
			if strings.Contains(n, " ") {
				// Exercise literal parsing in LIST.
				fmt.Fprintf(ss.w, "* LIST (%s) \"/\" {%d}\r\n%s\r\n", attrs, len(n), n)
			} else {
				fmt.Fprintf(ss.w, "* LIST (%s) \"/\" %s\r\n", attrs, imapQuote(n))
			}
		}
		s.mu.Unlock()
		ss.line(tag + " OK LIST completed")
	case "SELECT", "EXAMINE":
		if !needAuth() {
			return true
		}
		s.mu.Lock()
		b, ok := ss.acct.boxes[args[0]]
		s.mu.Unlock()
		if !ok {
			ss.line(tag + " NO [NONEXISTENT] no such mailbox")
			return true
		}
		ss.sel, ss.readOnly = b, verb == "EXAMINE"
		s.mu.Lock()
		fmt.Fprintf(ss.w, "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n* %d EXISTS\r\n* 0 RECENT\r\n", len(b.msgs))
		fmt.Fprintf(ss.w, "* OK [PERMANENTFLAGS (\\Deleted \\Seen \\*)] Limited\r\n* OK [UIDVALIDITY %d] UIDs valid\r\n* OK [UIDNEXT %d] Predicted next UID\r\n", b.validity, b.next)
		s.mu.Unlock()
		if verb == "EXAMINE" {
			ss.line(tag + " OK [READ-ONLY] EXAMINE completed")
		} else {
			ss.line(tag + " OK [READ-WRITE] SELECT completed")
		}
	case "UID SEARCH", "SEARCH":
		if !needSel() {
			return true
		}
		s.mu.Lock()
		var out []string
		for i, m := range ss.sel.msgs {
			if fakeMatch(m, args, uint32(i+1), ss.sel) {
				out = append(out, strconv.FormatUint(uint64(m.uid), 10))
			}
		}
		s.mu.Unlock()
		ss.line(strings.TrimSpace("* SEARCH " + strings.Join(out, " ")))
		ss.line(tag + " OK SEARCH completed")
	case "UID FETCH", "FETCH":
		if !needSel() {
			return true
		}
		byUID := verb == "UID FETCH"
		set, items := args[0], args[1:]
		ss.line("* 1 RECENT") // unsolicited noise the client must tolerate
		s.mu.Lock()
		for i, m := range ss.sel.msgs {
			seq := uint32(i + 1)
			key := seq
			if byUID {
				key = m.uid
			}
			if !inSet(key, set, ss.sel) {
				continue
			}
			ss.writeFetch(seq, m, items)
		}
		s.mu.Unlock()
		ss.line(tag + " OK FETCH completed")
	case "APPEND":
		if !needAuth() {
			return true
		}
		name := args[0]
		var flags []string
		i := 1
		if args[i] == "(" {
			for i++; args[i] != ")"; i++ {
				flags = append(flags, args[i])
			}
			i++
		}
		data := args[len(args)-1]
		s.mu.Lock()
		b, ok := ss.acct.boxes[name]
		if !ok {
			s.mu.Unlock()
			ss.line(tag + " NO [TRYCREATE] no such mailbox")
			return true
		}
		m := &fakeMsg{uid: b.next, flags: map[string]bool{}, raw: []byte(data), date: time.Now()}
		for _, f := range flags {
			m.flags[f] = true
		}
		b.next++
		b.msgs = append(b.msgs, m)
		s.mu.Unlock()
		if ss.has("UIDPLUS") {
			ss.line(fmt.Sprintf("%s OK [APPENDUID %d %d] APPEND completed", tag, b.validity, m.uid))
		} else {
			ss.line(tag + " OK APPEND completed")
		}
	case "UID STORE":
		if !needSel() {
			return true
		}
		if ss.readOnly {
			ss.line(tag + " NO mailbox is read-only")
			return true
		}
		op := strings.ToUpper(args[1])
		var flags []string
		for _, a := range args[2:] {
			if a != "(" && a != ")" {
				flags = append(flags, a)
			}
		}
		s.mu.Lock()
		for _, m := range ss.sel.msgs {
			if !inSet(m.uid, args[0], ss.sel) {
				continue
			}
			for _, f := range flags {
				if strings.HasPrefix(op, "+") {
					m.flags[f] = true
				} else {
					delete(m.flags, f)
				}
			}
		}
		s.mu.Unlock()
		ss.line(tag + " OK STORE completed")
	case "UID COPY", "UID MOVE":
		if !needSel() {
			return true
		}
		if verb == "UID MOVE" && !ss.has("MOVE") {
			ss.line(tag + " BAD unknown command")
			return true
		}
		s.mu.Lock()
		dst, ok := ss.acct.boxes[args[1]]
		if !ok {
			s.mu.Unlock()
			ss.line(tag + " NO [TRYCREATE] no such mailbox")
			return true
		}
		var keep []*fakeMsg
		for _, m := range ss.sel.msgs {
			if inSet(m.uid, args[0], ss.sel) {
				cp := &fakeMsg{uid: dst.next, flags: map[string]bool{}, raw: m.raw, date: m.date}
				for f := range m.flags {
					cp.flags[f] = true
				}
				dst.next++
				dst.msgs = append(dst.msgs, cp)
				if verb == "UID MOVE" {
					continue
				}
			}
			keep = append(keep, m)
		}
		ss.sel.msgs = keep
		s.mu.Unlock()
		ss.line(tag + " OK done")
	case "EXPUNGE", "UID EXPUNGE":
		if !needSel() {
			return true
		}
		s.mu.Lock()
		var keep []*fakeMsg
		for _, m := range ss.sel.msgs {
			if m.flags[`\Deleted`] && (verb == "EXPUNGE" || inSet(m.uid, args[0], ss.sel)) {
				continue
			}
			keep = append(keep, m)
		}
		ss.sel.msgs = keep
		s.mu.Unlock()
		ss.line(tag + " OK EXPUNGE completed")
	case "CREATE":
		if !needAuth() {
			return true
		}
		s.mu.Lock()
		ss.acct.boxes[args[0]] = &fakeBox{validity: 99, next: 1}
		s.mu.Unlock()
		ss.line(tag + " OK CREATE completed")
	case "LOGOUT":
		ss.line("* BYE logging out")
		ss.line(tag + " OK LOGOUT completed")
		return false
	default:
		ss.line(tag + " BAD unknown command " + verb)
	}
	return true
}

func (ss *imapSession) writeFetch(seq uint32, m *fakeMsg, items []string) {
	var parts []string
	var lits [][]byte
	for _, it := range items {
		u := strings.ToUpper(it)
		switch {
		case u == "(" || u == ")":
		case u == "UID":
			parts = append(parts, fmt.Sprintf("UID %d", m.uid))
		case u == "FLAGS":
			var fl []string
			for f := range m.flags {
				fl = append(fl, f)
			}
			sort.Strings(fl)
			parts = append(parts, "FLAGS ("+strings.Join(fl, " ")+")")
		case u == "INTERNALDATE":
			parts = append(parts, "INTERNALDATE "+imapQuote(m.date.Format("02-Jan-2006 15:04:05 -0700")))
		case u == "RFC822.SIZE":
			parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", len(m.raw)))
		case strings.HasPrefix(u, "BODY.PEEK[]<") || strings.HasPrefix(u, "BODY[]<"):
			spec := u[strings.Index(u, "<")+1 : len(u)-1]
			a, b, _ := strings.Cut(spec, ".")
			off, _ := strconv.Atoi(a)
			n, _ := strconv.Atoi(b)
			data := m.raw
			if off > len(data) {
				data = nil
			} else {
				data = data[off:]
			}
			if len(data) > n {
				data = data[:n]
			}
			parts = append(parts, fmt.Sprintf("BODY[]<%d> {%d}\r\n", off, len(data)))
			lits = append(lits, data)
		case u == "BODY.PEEK[]" || u == "BODY[]":
			parts = append(parts, fmt.Sprintf("BODY[] {%d}\r\n", len(m.raw)))
			lits = append(lits, m.raw)
		case u == "BODY.PEEK[HEADER]" || u == "BODY[HEADER]":
			h := m.raw
			if i := bytes.Index(h, []byte("\r\n\r\n")); i >= 0 {
				h = h[:i+4]
			}
			parts = append(parts, fmt.Sprintf("BODY[HEADER] {%d}\r\n", len(h)))
			lits = append(lits, h)
		}
	}
	fmt.Fprintf(ss.w, "* %d FETCH (", seq)
	li := 0
	for i, p := range parts {
		if i > 0 {
			ss.w.WriteString(" ")
		}
		ss.w.WriteString(p)
		if strings.HasSuffix(p, "}\r\n") {
			ss.w.Write(lits[li])
			li++
		}
	}
	ss.w.WriteString(")\r\n")
}

// inSet reports whether n is in an IMAP sequence set ("1,3:5,7:*").
func inSet(n uint32, set string, b *fakeBox) bool {
	max := uint32(0)
	for _, m := range b.msgs {
		if m.uid > max {
			max = m.uid
		}
	}
	if uint32(len(b.msgs)) > max {
		max = uint32(len(b.msgs))
	}
	for _, r := range strings.Split(set, ",") {
		lo, hi, isRange := strings.Cut(r, ":")
		parse := func(s string) uint32 {
			if s == "*" {
				return max
			}
			v, _ := strconv.ParseUint(s, 10, 32)
			return uint32(v)
		}
		a := parse(lo)
		z := a
		if isRange {
			z = parse(hi)
		}
		if a > z {
			a, z = z, a
		}
		if n >= a && n <= z {
			return true
		}
	}
	return false
}

func fakeMatch(m *fakeMsg, crit []string, seq uint32, b *fakeBox) bool {
	hdr, _ := mail.ReadMessage(bytes.NewReader(m.raw))
	get := func(k string) string {
		if hdr == nil {
			return ""
		}
		return strings.ToLower(decodeHeader(hdr.Header.Get(k)))
	}
	contains := func(hay, needle string) bool { return strings.Contains(hay, strings.ToLower(needle)) }
	for i := 0; i < len(crit); i++ {
		switch strings.ToUpper(crit[i]) {
		case "ALL":
		case "CHARSET":
			i++
		case "UNSEEN":
			if m.flags[`\Seen`] {
				return false
			}
		case "SEEN":
			if !m.flags[`\Seen`] {
				return false
			}
		case "FLAGGED":
			if !m.flags[`\Flagged`] {
				return false
			}
		case "UID":
			i++
			if !inSet(m.uid, crit[i], b) {
				return false
			}
		case "FROM", "TO", "CC", "SUBJECT":
			i++
			if !contains(get(crit[i-1]), crit[i]) {
				return false
			}
		case "TEXT":
			i++
			if !contains(strings.ToLower(string(m.raw)), crit[i]) {
				return false
			}
		case "HEADER":
			i += 2
			if !contains(get(crit[i-1]), crit[i]) {
				return false
			}
		case "SINCE", "BEFORE":
			i++
			d, err := time.Parse("2-Jan-2006", crit[i])
			if err != nil || hdr == nil {
				return false
			}
			md, _ := hdr.Header.Date()
			if strings.EqualFold(crit[i-1], "SINCE") && md.Before(d) || strings.EqualFold(crit[i-1], "BEFORE") && !md.Before(d) {
				return false
			}
		default:
			if inSet(seq, crit[i], b) {
				continue
			}
			return false
		}
	}
	return true
}
