package email

// A minimal IMAP4rev1 (RFC 3501) client: the subset the email tools need.
//
// Parsing is deliberately tolerant. Status responses (OK/NO/BAD/BYE/PREAUTH)
// and continuation requests keep their free-form text raw; everything else
// (CAPABILITY, LIST, SEARCH, FLAGS, FETCH, ...) is parsed into a small value
// tree: atoms (atom), strings (quoted or literal, []byte), parenthesised
// lists ([]any). Literals ({n}\r\n<n bytes>) may appear anywhere a string
// can, including in the middle of a FETCH response.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parser limits; a hostile or broken server cannot make us allocate more.
const (
	maxLiteral   = 50 << 20 // one literal (a whole message)
	maxAtom      = 1 << 20
	maxDepth     = 32
	maxListItems = 1 << 20
	maxRawLine   = 1 << 20
)

// atom is an IMAP atom (including NIL, numbers, flags and section specs
// such as BODY[HEADER.FIELDS (FROM)]<0>).
type atom string

// response is one server response.
type response struct {
	tag    string // "*", "+", or the command tag
	num    uint32 // the number in "* <n> EXISTS|EXPUNGE|FETCH"
	hasNum bool
	typ    string // upper-cased response type: OK, NO, CAPABILITY, FETCH, ...
	text   string // raw text of status responses and continuations
	fields []any  // parsed remainder of data responses
}

// imapReader parses responses from a buffered stream.
type imapReader struct{ r *bufio.Reader }

func (p *imapReader) peek() (byte, error) {
	b, err := p.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (p *imapReader) skipSpaces() error {
	for {
		c, err := p.peek()
		if err != nil {
			return err
		}
		if c != ' ' {
			return nil
		}
		p.r.ReadByte()
	}
}

// readWord reads up to (not including) the next space or line end.
func (p *imapReader) readWord() (string, error) {
	var sb strings.Builder
	for {
		c, err := p.peek()
		if err != nil {
			return "", err
		}
		if c == ' ' || c == '\r' || c == '\n' {
			return sb.String(), nil
		}
		if sb.Len() >= maxAtom {
			return "", errors.New("imap: word too long")
		}
		p.r.ReadByte()
		sb.WriteByte(c)
	}
}

// readEOL consumes "\r\n" (or a bare "\n").
func (p *imapReader) readEOL() error {
	c, err := p.r.ReadByte()
	if err != nil {
		return err
	}
	if c == '\r' {
		if c, err = p.r.ReadByte(); err != nil {
			return err
		}
	}
	if c != '\n' {
		return fmt.Errorf("imap: expected end of line, got %q", c)
	}
	return nil
}

var trailingLiteral = regexp.MustCompile(`\{(\d+)\+?\}$`)

// readRestRaw returns the rest of the line (without CRLF). A trailing
// literal marker is honoured: its bytes are read and appended, and the line
// continues after it.
func (p *imapReader) readRestRaw() (string, error) {
	var sb strings.Builder
	for {
		line, err := p.r.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
		sb.Write(line)
		if sb.Len() > maxRawLine {
			return "", errors.New("imap: response line too long")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		s := strings.TrimRight(sb.String(), "\r\n")
		m := trailingLiteral.FindStringSubmatch(s)
		if m == nil {
			return s, nil
		}
		n, _ := strconv.Atoi(m[1])
		if n > maxRawLine {
			return "", errors.New("imap: literal in status text too long")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(p.r, buf); err != nil {
			return "", err
		}
		sb.Reset()
		sb.WriteString(s[:len(s)-len(m[0])])
		sb.Write(buf)
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// readResponse reads one complete response.
func (p *imapReader) readResponse() (*response, error) {
	tag, err := p.readWord()
	if err != nil {
		return nil, err
	}
	r := &response{tag: tag}
	if tag == "" {
		// Blank line or leading garbage; skip it.
		if _, err := p.readRestRaw(); err != nil {
			return nil, err
		}
		return p.readResponse()
	}
	if err := p.skipSpaces(); err != nil {
		return nil, err
	}
	if tag == "+" {
		r.typ = "+"
		r.text, err = p.readRestRaw()
		return r, err
	}
	w, err := p.readWord()
	if err != nil {
		return nil, err
	}
	if tag == "*" && isDigits(w) {
		n, _ := strconv.ParseUint(w, 10, 32)
		r.num, r.hasNum = uint32(n), true
		if err := p.skipSpaces(); err != nil {
			return nil, err
		}
		if w, err = p.readWord(); err != nil {
			return nil, err
		}
	}
	r.typ = strings.ToUpper(w)
	switch {
	case r.typ == "OK" || r.typ == "NO" || r.typ == "BAD" || r.typ == "BYE" || r.typ == "PREAUTH" || tag != "*":
		if err := p.skipSpaces(); err != nil {
			return nil, err
		}
		r.text, err = p.readRestRaw()
		return r, err
	default:
		r.fields, err = p.readFields()
		return r, err
	}
}

// readFields parses values up to the end of the line.
func (p *imapReader) readFields() ([]any, error) {
	var out []any
	for {
		if err := p.skipSpaces(); err != nil {
			return nil, err
		}
		c, err := p.peek()
		if err != nil {
			return nil, err
		}
		if c == '\r' || c == '\n' {
			return out, p.readEOL()
		}
		if c == ')' { // stray close paren: ignore
			p.r.ReadByte()
			continue
		}
		v, err := p.readValue(0)
		if err != nil {
			return nil, err
		}
		if len(out) >= maxListItems {
			return nil, errors.New("imap: too many items")
		}
		out = append(out, v)
	}
}

func (p *imapReader) readValue(depth int) (any, error) {
	c, err := p.peek()
	if err != nil {
		return nil, err
	}
	switch c {
	case '(':
		if depth >= maxDepth {
			return nil, errors.New("imap: lists nested too deeply")
		}
		p.r.ReadByte()
		list := []any{}
		for {
			if err := p.skipSpaces(); err != nil {
				return nil, err
			}
			c, err := p.peek()
			if err != nil {
				return nil, err
			}
			if c == ')' {
				p.r.ReadByte()
				return list, nil
			}
			if c == '\r' || c == '\n' {
				return nil, errors.New("imap: unterminated list")
			}
			v, err := p.readValue(depth + 1)
			if err != nil {
				return nil, err
			}
			if len(list) >= maxListItems {
				return nil, errors.New("imap: too many items")
			}
			list = append(list, v)
		}
	case '"':
		return p.readQuoted()
	case '{':
		return p.readLiteral()
	case '~': // literal8 (RFC 3516)
		p.r.ReadByte()
		if c, err := p.peek(); err == nil && c == '{' {
			return p.readLiteral()
		}
		return atom("~"), nil
	default:
		return p.readAtom()
	}
}

func (p *imapReader) readQuoted() ([]byte, error) {
	p.r.ReadByte() // opening quote
	var b []byte
	for {
		c, err := p.r.ReadByte()
		if err != nil {
			return nil, err
		}
		switch c {
		case '"':
			return b, nil
		case '\\':
			if c, err = p.r.ReadByte(); err != nil {
				return nil, err
			}
		case '\r', '\n':
			return nil, errors.New("imap: line break in quoted string")
		}
		if len(b) >= maxAtom {
			return nil, errors.New("imap: quoted string too long")
		}
		b = append(b, c)
	}
}

func (p *imapReader) readLiteral() ([]byte, error) {
	p.r.ReadByte() // '{'
	var digits []byte
	for {
		c, err := p.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if c == '}' {
			break
		}
		if c == '+' {
			continue
		}
		if c < '0' || c > '9' || len(digits) > 10 {
			return nil, errors.New("imap: malformed literal")
		}
		digits = append(digits, c)
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil {
		return nil, errors.New("imap: malformed literal")
	}
	if n > maxLiteral {
		return nil, fmt.Errorf("imap: literal of %d bytes exceeds the %d byte limit", n, maxLiteral)
	}
	if err := p.readEOL(); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// readAtom reads an atom; "[...]" sections (which may contain spaces and
// parentheses) are part of it.
func (p *imapReader) readAtom() (any, error) {
	var sb strings.Builder
	bracket := 0
	for {
		c, err := p.peek()
		if err != nil {
			return nil, err
		}
		if c == '\r' || c == '\n' {
			break
		}
		if bracket == 0 && (c == ' ' || c == '(' || c == ')' || c == '"' || c == '{') {
			break
		}
		if c == '[' {
			bracket++
		} else if c == ']' && bracket > 0 {
			bracket--
		}
		if sb.Len() >= maxAtom {
			return nil, errors.New("imap: atom too long")
		}
		p.r.ReadByte()
		sb.WriteByte(c)
	}
	if sb.Len() == 0 {
		c, _ := p.r.ReadByte()
		return nil, fmt.Errorf("imap: unexpected %q", c)
	}
	return atom(sb.String()), nil
}

// asString returns v as a string: string values, and atoms other than NIL.
func asString(v any) (string, bool) {
	switch x := v.(type) {
	case []byte:
		return string(x), true
	case atom:
		if strings.EqualFold(string(x), "NIL") {
			return "", false
		}
		return string(x), true
	}
	return "", false
}

// lit is a command argument sent as a literal.
type lit []byte

// imapConn is one authenticated-or-not IMAP connection.
type imapConn struct {
	conn    net.Conn
	r       *imapReader
	w       *bufio.Writer
	tagN    int
	caps    map[string]bool
	timeout time.Duration
	stop    func() bool // detaches the ctx-cancel watcher
}

func newIMAPConn(conn net.Conn, timeout time.Duration) *imapConn {
	return &imapConn{conn: conn, r: &imapReader{r: bufio.NewReaderSize(conn, 64<<10)}, w: bufio.NewWriter(conn),
		caps: map[string]bool{}, timeout: timeout}
}

func (c *imapConn) deadline() { _ = c.conn.SetDeadline(time.Now().Add(c.timeout)) }

// greet reads the server greeting.
func (c *imapConn) greet() error {
	c.deadline()
	r, err := c.r.readResponse()
	if err != nil {
		return fmt.Errorf("imap: greeting: %w", err)
	}
	if r.tag != "*" || (r.typ != "OK" && r.typ != "PREAUTH") {
		return fmt.Errorf("imap: server refused connection: %s %s", r.typ, r.text)
	}
	return nil
}

// IMAPError is a NO/BAD tagged reply.
type IMAPError struct {
	Command string
	Status  string
	Text    string
}

func (e *IMAPError) Error() string {
	return fmt.Sprintf("imap %s failed: %s %s", e.Command, e.Status, e.Text)
}

// cmd sends one command made of raw strings and literals and reads
// responses up to its tagged completion. It returns the untagged responses
// and the tagged one; a NO/BAD completion is returned as *IMAPError. Only
// the command name (never the arguments, which may carry credentials)
// appears in errors.
func (c *imapConn) cmd(args ...any) ([]*response, *response, error) {
	c.tagN++
	tag := fmt.Sprintf("B%03d", c.tagN)
	name := "command"
	if len(args) > 0 {
		if s, ok := args[0].(string); ok {
			name, _, _ = strings.Cut(s, " ")
			if strings.EqualFold(name, "UID") {
				if f := strings.Fields(s); len(f) > 1 {
					name = "UID " + f[1]
				}
			}
		}
	}
	c.deadline()
	var untagged []*response
	c.w.WriteString(tag + " ")
	for _, a := range args {
		switch v := a.(type) {
		case string:
			c.w.WriteString(v)
		case lit:
			if c.caps["LITERAL+"] {
				fmt.Fprintf(c.w, "{%d+}\r\n", len(v))
				c.w.Write(v)
				continue
			}
			fmt.Fprintf(c.w, "{%d}\r\n", len(v))
			if err := c.w.Flush(); err != nil {
				return nil, nil, fmt.Errorf("imap %s: %w", name, err)
			}
			for {
				r, err := c.r.readResponse()
				if err != nil {
					return nil, nil, fmt.Errorf("imap %s: %w", name, err)
				}
				if r.tag == "+" {
					break
				}
				if r.tag == tag {
					return untagged, r, &IMAPError{Command: name, Status: r.typ, Text: r.text}
				}
				untagged = append(untagged, r)
			}
			c.deadline()
			c.w.Write(v)
		default:
			panic(fmt.Sprintf("imap: bad argument type %T", a))
		}
	}
	c.w.WriteString("\r\n")
	if err := c.w.Flush(); err != nil {
		return nil, nil, fmt.Errorf("imap %s: %w", name, err)
	}
	for {
		r, err := c.r.readResponse()
		if err != nil {
			return untagged, nil, fmt.Errorf("imap %s: %w", name, err)
		}
		switch {
		case r.tag == tag:
			if r.typ != "OK" {
				return untagged, r, &IMAPError{Command: name, Status: r.typ, Text: r.text}
			}
			return untagged, r, nil
		case r.tag == "+":
			// Unexpected continuation; answer with an empty line so the
			// server can finish the command.
			c.w.WriteString("\r\n")
			c.w.Flush()
		default:
			untagged = append(untagged, r)
		}
	}
}

// quote encodes s as an IMAP astring argument: a quoted string, or a
// literal when s has characters a quoted string cannot carry.
func quote(s string) any {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			return lit(s)
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (c *imapConn) capability() error {
	untagged, _, err := c.cmd("CAPABILITY")
	if err != nil {
		return err
	}
	c.caps = map[string]bool{}
	for _, r := range untagged {
		if r.typ != "CAPABILITY" {
			continue
		}
		for _, f := range r.fields {
			if s, ok := asString(f); ok {
				c.caps[strings.ToUpper(s)] = true
			}
		}
	}
	return nil
}

func (c *imapConn) startTLS(ctx context.Context, cfg *tls.Config) error {
	if !c.caps["STARTTLS"] {
		return errors.New("imap: server does not offer STARTTLS; refusing to log in without TLS")
	}
	if _, _, err := c.cmd("STARTTLS"); err != nil {
		return err
	}
	tc := tls.Client(c.conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("imap: starttls: %w", err)
	}
	c.conn = tc
	c.r = &imapReader{r: bufio.NewReaderSize(tc, 64<<10)}
	c.w = bufio.NewWriter(tc)
	return c.capability()
}

func (c *imapConn) login(user, pass string) error {
	if c.caps["LOGINDISABLED"] {
		return errors.New("imap: server disabled LOGIN")
	}
	_, _, err := c.cmd("LOGIN ", quote(user), " ", quote(pass))
	return err
}

// xoauth2 builds the SASL XOAUTH2 initial response.
func xoauth2(user, token string) string {
	return base64.StdEncoding.EncodeToString([]byte("user=" + user + "\x01auth=Bearer " + token + "\x01\x01"))
}

func (c *imapConn) authenticateXOAUTH2(user, token string) error {
	c.tagN++
	tag := fmt.Sprintf("B%03d", c.tagN)
	ir := xoauth2(user, token)
	c.deadline()
	if c.caps["SASL-IR"] {
		c.w.WriteString(tag + " AUTHENTICATE XOAUTH2 " + ir + "\r\n")
	} else {
		c.w.WriteString(tag + " AUTHENTICATE XOAUTH2\r\n")
	}
	if err := c.w.Flush(); err != nil {
		return fmt.Errorf("imap AUTHENTICATE: %w", err)
	}
	sentIR := c.caps["SASL-IR"]
	for {
		r, err := c.r.readResponse()
		if err != nil {
			return fmt.Errorf("imap AUTHENTICATE: %w", err)
		}
		switch {
		case r.tag == "+":
			if !sentIR {
				c.w.WriteString(ir + "\r\n")
				sentIR = true
			} else {
				// An error challenge (base64 JSON); reply empty to get the
				// tagged NO.
				c.w.WriteString("\r\n")
			}
			if err := c.w.Flush(); err != nil {
				return fmt.Errorf("imap AUTHENTICATE: %w", err)
			}
		case r.tag == tag:
			if r.typ != "OK" {
				return &IMAPError{Command: "AUTHENTICATE", Status: r.typ, Text: r.text}
			}
			return nil
		}
	}
}

func (c *imapConn) logout() {
	if c == nil || c.conn == nil {
		return
	}
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _, _ = c.cmd("LOGOUT")
	c.conn.Close()
	if c.stop != nil {
		c.stop()
	}
}

// mailbox is one LIST entry. Name is the raw (modified UTF-7) name.
type mailbox struct {
	Name  string
	Delim string
	Attrs []string
}

func (m mailbox) has(attr string) bool {
	for _, a := range m.Attrs {
		if strings.EqualFold(a, attr) {
			return true
		}
	}
	return false
}

func (c *imapConn) list() ([]mailbox, error) {
	untagged, _, err := c.cmd(`LIST "" "*"`)
	if err != nil {
		return nil, err
	}
	var out []mailbox
	for _, r := range untagged {
		if r.typ != "LIST" || len(r.fields) < 3 {
			continue
		}
		var m mailbox
		if attrs, ok := r.fields[0].([]any); ok {
			for _, a := range attrs {
				if s, ok := asString(a); ok {
					m.Attrs = append(m.Attrs, s)
				}
			}
		}
		m.Delim, _ = asString(r.fields[1])
		name, ok := asString(r.fields[2])
		if !ok {
			continue
		}
		m.Name = name
		out = append(out, m)
	}
	return out, nil
}

// mboxStatus is what SELECT/EXAMINE report.
type mboxStatus struct {
	Exists      uint32
	UIDValidity uint32
	UIDNext     uint32
}

var respCodeRe = regexp.MustCompile(`(?i)\[(UIDVALIDITY|UIDNEXT) (\d+)\]`)

// selectFolder opens a folder (EXAMINE when readOnly).
func (c *imapConn) selectFolder(name string, readOnly bool) (mboxStatus, error) {
	verb := "SELECT "
	if readOnly {
		verb = "EXAMINE "
	}
	untagged, _, err := c.cmd(verb, quote(name))
	if err != nil {
		return mboxStatus{}, err
	}
	var st mboxStatus
	for _, r := range untagged {
		if r.typ == "EXISTS" && r.hasNum {
			st.Exists = r.num
		}
		if r.typ == "OK" {
			for _, m := range respCodeRe.FindAllStringSubmatch(r.text, -1) {
				n, _ := strconv.ParseUint(m[2], 10, 32)
				if strings.EqualFold(m[1], "UIDVALIDITY") {
					st.UIDValidity = uint32(n)
				} else {
					st.UIDNext = uint32(n)
				}
			}
		}
	}
	return st, nil
}

// uidSearch runs UID SEARCH with the given criteria arguments (raw strings
// and literals, already space-separated by the caller).
func (c *imapConn) uidSearch(criteria ...any) ([]uint32, error) {
	untagged, _, err := c.cmd(append([]any{"UID SEARCH "}, criteria...)...)
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, r := range untagged {
		if r.typ != "SEARCH" {
			continue
		}
		for _, f := range r.fields {
			s, _ := asString(f)
			if n, err := strconv.ParseUint(s, 10, 32); err == nil {
				out = append(out, uint32(n))
			}
		}
	}
	return out, nil
}

// fetched is one message from a FETCH response.
type fetched struct {
	UID          uint32
	Flags        []string
	InternalDate string
	Size         int64
	Body         []byte // BODY[] / BODY[]<0> / BODY[HEADER] content
}

func (f fetched) hasFlag(flag string) bool {
	for _, x := range f.Flags {
		if strings.EqualFold(x, flag) {
			return true
		}
	}
	return false
}

// fetch runs FETCH (or UID FETCH when byUID) for set with the given items
// and returns the messages that carried a UID.
func (c *imapConn) fetch(set string, byUID bool, items string) ([]fetched, error) {
	verb := "FETCH "
	if byUID {
		verb = "UID FETCH "
	}
	untagged, _, err := c.cmd(verb + set + " (" + items + ")")
	if err != nil {
		return nil, err
	}
	var out []fetched
	for _, r := range untagged {
		if r.typ != "FETCH" || len(r.fields) == 0 {
			continue
		}
		kv, ok := r.fields[0].([]any)
		if !ok {
			continue
		}
		var f fetched
		for i := 0; i+1 < len(kv); i += 2 {
			k, _ := asString(kv[i])
			k = strings.ToUpper(k)
			v := kv[i+1]
			switch {
			case k == "UID":
				s, _ := asString(v)
				n, _ := strconv.ParseUint(s, 10, 32)
				f.UID = uint32(n)
			case k == "FLAGS":
				if l, ok := v.([]any); ok {
					for _, x := range l {
						if s, ok := asString(x); ok {
							f.Flags = append(f.Flags, s)
						}
					}
				}
			case k == "INTERNALDATE":
				f.InternalDate, _ = asString(v)
			case k == "RFC822.SIZE":
				s, _ := asString(v)
				f.Size, _ = strconv.ParseInt(s, 10, 64)
			case strings.HasPrefix(k, "BODY[") || strings.HasPrefix(k, "RFC822"):
				if b, ok := v.([]byte); ok {
					f.Body = b
				}
			}
		}
		if f.UID != 0 {
			out = append(out, f)
		}
	}
	return out, nil
}

var appendUIDRe = regexp.MustCompile(`(?i)\[APPENDUID (\d+) (\d+)\]`)

// appendMsg APPENDs msg to folder with flags; it returns the new UID when
// the server reports it (UIDPLUS), else 0.
func (c *imapConn) appendMsg(folder string, flags string, msg []byte) (uint32, error) {
	_, done, err := c.cmd("APPEND ", quote(folder), " ("+flags+") ", lit(msg))
	if err != nil {
		return 0, err
	}
	if m := appendUIDRe.FindStringSubmatch(done.text); m != nil {
		n, _ := strconv.ParseUint(m[2], 10, 32)
		return uint32(n), nil
	}
	return 0, nil
}

func (c *imapConn) store(uid uint32, op, flags string) error {
	_, _, err := c.cmd(fmt.Sprintf("UID STORE %d %s (%s)", uid, op, flags))
	return err
}

// move moves uid from the selected folder to dest: MOVE when supported,
// else COPY + \Deleted + (UID) EXPUNGE.
func (c *imapConn) move(uid uint32, dest string) error {
	if c.caps["MOVE"] {
		_, _, err := c.cmd(fmt.Sprintf("UID MOVE %d ", uid), quote(dest))
		return err
	}
	if _, _, err := c.cmd(fmt.Sprintf("UID COPY %d ", uid), quote(dest)); err != nil {
		return err
	}
	if err := c.store(uid, "+FLAGS.SILENT", `\Deleted`); err != nil {
		return err
	}
	if c.caps["UIDPLUS"] {
		_, _, err := c.cmd(fmt.Sprintf("UID EXPUNGE %d", uid))
		return err
	}
	_, _, err := c.cmd("EXPUNGE")
	return err
}

func (c *imapConn) create(name string) error {
	_, _, err := c.cmd("CREATE ", quote(name))
	return err
}
