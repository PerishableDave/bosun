package email

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ---- inbound: parsing ----

const (
	maxPartText = 2 << 20 // decoded text read from one part
	maxParts    = 200
	maxMIMEDep  = 20
)

// attachment is listed by name and size only; its content is never
// returned.
type attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// parsedMessage is the readable view of a message.
type parsedMessage struct {
	From, To, Cc, ReplyTo string
	Subject, Date         string
	MessageID, References string
	plain, html           []string
	Attachments           []attachment
	parts                 int
}

// Text returns the body: text/plain parts when present, else the HTML parts
// converted to text.
func (p *parsedMessage) Text() string {
	if s := strings.TrimSpace(strings.Join(p.plain, "\n\n")); s != "" {
		return cleanText(s)
	}
	var out []string
	for _, h := range p.html {
		out = append(out, htmlToText(h))
	}
	return cleanText(strings.Join(out, "\n\n"))
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// decodeHeader decodes RFC 2047 encoded words, tolerating junk.
func decodeHeader(s string) string {
	if s == "" {
		return ""
	}
	d, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		d = s
	}
	return cleanLine(d)
}

// formatAddresses renders an address header as "Name <addr>, ...".
func formatAddresses(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	p := mail.AddressParser{WordDecoder: wordDecoder}
	list, err := p.ParseList(s)
	if err != nil {
		return decodeHeader(s)
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		if a.Name != "" {
			out = append(out, fmt.Sprintf("%s <%s>", cleanLine(a.Name), a.Address))
		} else {
			out = append(out, a.Address)
		}
	}
	return strings.Join(out, ", ")
}

// parseMessage parses a raw message (possibly truncated, e.g. from a
// partial fetch) as far as it can.
func parseMessage(raw []byte) *parsedMessage {
	p := &parsedMessage{}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	var get func(string) string
	var body io.Reader
	if err != nil {
		// Malformed header block: treat everything as a plain body after
		// the first blank line.
		_, rest, found := bytes.Cut(raw, []byte("\r\n\r\n"))
		if !found {
			_, rest, _ = bytes.Cut(raw, []byte("\n\n"))
		}
		get = func(string) string { return "" }
		body = bytes.NewReader(rest)
	} else {
		get = m.Header.Get
		body = m.Body
	}
	p.From = formatAddresses(get("From"))
	p.To = formatAddresses(get("To"))
	p.Cc = formatAddresses(get("Cc"))
	p.ReplyTo = formatAddresses(get("Reply-To"))
	p.Subject = decodeHeader(get("Subject"))
	p.MessageID = strings.TrimSpace(get("Message-Id"))
	p.References = strings.Join(strings.Fields(get("References")), " ")
	if d := get("Date"); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			p.Date = t.Format(time.RFC3339)
		} else {
			p.Date = cleanLine(d)
		}
	}
	p.walk(get, body, 0)
	return p
}

func (p *parsedMessage) walk(get func(string) string, body io.Reader, depth int) {
	p.parts++
	if p.parts > maxParts {
		return
	}
	ct, params, err := mime.ParseMediaType(get("Content-Type"))
	if ct == "" || (err != nil && err != mime.ErrInvalidMediaParameter) {
		ct, params = "text/plain", map[string]string{}
	}
	disp, dparams, _ := mime.ParseMediaType(get("Content-Disposition"))
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	name = decodeHeader(name)
	body = decodeTransfer(get("Content-Transfer-Encoding"), body)

	if strings.HasPrefix(ct, "multipart/") && params["boundary"] != "" {
		if depth >= maxMIMEDep {
			return
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				return // end, or truncated/malformed: keep what we have
			}
			p.walk(part.Header.Get, part, depth+1)
		}
	}
	isBodyText := (ct == "text/plain" || ct == "text/html") && disp != "attachment" && name == ""
	if !isBodyText {
		n, _ := io.Copy(io.Discard, body)
		if name == "" {
			name = "(unnamed " + ct + ")"
		}
		p.Attachments = append(p.Attachments, attachment{Name: name, ContentType: ct, Size: n})
		return
	}
	b, _ := io.ReadAll(io.LimitReader(body, maxPartText)) // partial data on error is fine
	s := decodeCharset(b, params["charset"])
	if ct == "text/html" {
		p.html = append(p.html, s)
	} else {
		p.plain = append(p.plain, s)
	}
}

// decodeTransfer undoes Content-Transfer-Encoding. (multipart.Part already
// decodes quoted-printable and removes the header.)
func decodeTransfer(cte string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &b64Filter{r: r})
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// b64Filter drops bytes outside the base64 alphabet (line breaks, stray
// spaces) so sloppy encoders still decode.
type b64Filter struct{ r io.Reader }

func (f *b64Filter) Read(p []byte) (int, error) {
	for {
		n, err := f.r.Read(p)
		j := 0
		for _, c := range p[:n] {
			if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
				p[j] = c
				j++
			}
		}
		if j > 0 || err != nil {
			return j, err
		}
	}
}

// cp1252 maps 0x80..0x9F in windows-1252 (0 = undefined).
var cp1252 = [32]rune{
	0x20AC, 0, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017D, 0,
	0, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0, 0x017E, 0x0178,
}

func isLatin(label string) bool {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "l1", "windows-1252", "cp1252", "iso-8859-15", "latin9":
		return true
	}
	return false
}

func latinToUTF8(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c >= 0x80 && c < 0xA0 && cp1252[c-0x80] != 0 {
			sb.WriteRune(cp1252[c-0x80])
		} else {
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

// charsetReader supports UTF-8/ASCII and the Latin-1 family natively and
// passes anything else through (invalid UTF-8 is replaced later).
func charsetReader(label string, r io.Reader) (io.Reader, error) {
	if isLatin(label) {
		b, err := io.ReadAll(io.LimitReader(r, maxPartText))
		if err != nil {
			return nil, err
		}
		return strings.NewReader(latinToUTF8(b)), nil
	}
	return r, nil
}

func decodeCharset(b []byte, label string) string {
	if isLatin(label) {
		return latinToUTF8(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// invisible reports runes that hide text from a human reader (zero-width
// and bidi controls), a common prompt-injection trick.
func invisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2064,
		r >= 0x2066 && r <= 0x2069, r == 0xFEFF, r == 0x00AD:
		return true
	}
	return false
}

// cleanText removes control and invisible characters (keeping newlines and
// tabs), normalises line endings and collapses runs of blank lines.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			sb.WriteRune(r)
		case r == '\r':
			sb.WriteRune('\n')
		case unicode.IsControl(r) || invisible(r):
		default:
			sb.WriteRune(r)
		}
	}
	lines := strings.Split(sb.String(), "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// cleanLine flattens s to one clean line.
func cleanLine(s string) string { return strings.Join(strings.Fields(cleanText(s)), " ") }

// snippet returns the first n runes of text on one line.
func snippet(text string, n int) string { return truncateRunes(cleanLine(text), n, "…") }

func truncateRunes(s string, n int, suffix string) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for k := range s {
		if i == n {
			return s[:k] + suffix
		}
		i++
	}
	return s
}

// ---- HTML to text ----

var htmlBlockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "ul": true, "ol": true, "tr": true, "table": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "blockquote": true, "pre": true,
	"hr": true, "section": true, "article": true, "header": true, "footer": true, "title": true, "td": false,
}

var htmlSkipTags = map[string]bool{"script": true, "style": true, "head": true, "noscript": true, "template": true, "svg": true}

// htmlToText strips tags: script/style/head contents and comments are
// dropped, block tags become line breaks, entities are decoded.
func htmlToText(s string) string {
	var sb strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '<' {
			sb.WriteByte(c)
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "<!--") {
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			sb.WriteString(s[i:])
			break
		}
		tag := s[i+1 : i+end]
		i += end + 1
		closing := strings.HasPrefix(tag, "/")
		name := htmlTagName(strings.TrimPrefix(tag, "/"))
		if name == "" {
			if !strings.HasPrefix(tag, "!") && !strings.HasPrefix(tag, "?") {
				sb.WriteString("<" + tag + ">") // a literal "<" in text
			}
			continue
		}
		if htmlSkipTags[name] && !closing && !strings.HasSuffix(tag, "/") {
			idx := strings.Index(strings.ToLower(s[i:]), "</"+name)
			if idx < 0 {
				i = len(s)
				break
			}
			i += idx
			if e := strings.IndexByte(s[i:], '>'); e >= 0 {
				i += e + 1
			} else {
				i = len(s)
			}
			continue
		}
		switch {
		case htmlBlockTags[name]:
			sb.WriteByte('\n')
		case name == "td" || name == "th":
			sb.WriteByte('\t')
		default:
			sb.WriteByte(' ')
		}
	}
	text := html.UnescapeString(sb.String())
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.Join(strings.FieldsFunc(l, func(r rune) bool { return unicode.IsSpace(r) || r == 0xa0 }), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return cleanText(strings.Join(lines, "\n"))
}

func htmlTagName(tag string) string {
	n := 0
	for n < len(tag) {
		c := tag[n]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (n > 0 && c >= '0' && c <= '9') {
			n++
			continue
		}
		break
	}
	return strings.ToLower(tag[:n])
}

// ---- outbound: building ----

// outgoing is a message to send or save as a draft.
type outgoing struct {
	From       mail.Address
	To, Cc     []*mail.Address
	Bcc        []*mail.Address
	Subject    string
	Body       string
	InReplyTo  string
	References string
	MessageID  string
	Date       time.Time
}

// newMessageID returns "<random@domain>".
func newMessageID(from string) string {
	domain := "bosun.local"
	if _, d, ok := strings.Cut(from, "@"); ok && d != "" {
		domain = d
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "<" + hex.EncodeToString(b[:]) + "@" + domain + ">"
}

func joinAddrs(list []*mail.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = a.String()
	}
	return strings.Join(parts, ",\r\n ")
}

// foldIDs folds a space-separated list of message IDs.
func foldIDs(s string) string { return strings.Join(strings.Fields(s), "\r\n ") }

func encodeSubject(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// bytes renders the message (RFC 5322, quoted-printable UTF-8 text). Bcc is
// only included when includeBcc (the Sent copy), never in what goes over
// SMTP.
func (o *outgoing) bytes(includeBcc bool) []byte {
	var b bytes.Buffer
	h := func(k, v string) {
		if v != "" {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	h("Date", o.Date.Format(time.RFC1123Z))
	h("From", o.From.String())
	h("To", joinAddrs(o.To))
	h("Cc", joinAddrs(o.Cc))
	if includeBcc {
		h("Bcc", joinAddrs(o.Bcc))
	}
	h("Subject", encodeSubject(o.Subject))
	h("Message-ID", o.MessageID)
	h("In-Reply-To", o.InReplyTo)
	h("References", foldIDs(o.References))
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := strings.ReplaceAll(o.Body, "\r\n", "\n")
	qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
	qp.Close()
	if !bytes.HasSuffix(b.Bytes(), []byte("\r\n")) {
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// ---- modified UTF-7 (RFC 3501 §5.1.3) mailbox names ----

var mutf7 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// decodeMailbox converts a raw mailbox name to UTF-8 (unchanged if it is
// not valid modified UTF-7).
func decodeMailbox(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			sb.WriteByte(s[i])
			continue
		}
		end := strings.IndexByte(s[i:], '-')
		if end < 0 {
			return s
		}
		chunk := s[i+1 : i+end]
		i += end
		if chunk == "" {
			sb.WriteByte('&')
			continue
		}
		raw, err := mutf7.DecodeString(chunk)
		if err != nil || len(raw)%2 != 0 {
			return s
		}
		u := make([]uint16, len(raw)/2)
		for k := range u {
			u[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
		}
		sb.WriteString(string(utf16.Decode(u)))
	}
	return sb.String()
}

// encodeMailbox converts a UTF-8 mailbox name to modified UTF-7.
func encodeMailbox(s string) string {
	var sb strings.Builder
	var run []rune
	flush := func() {
		if len(run) == 0 {
			return
		}
		u := utf16.Encode(run)
		raw := make([]byte, 2*len(u))
		for k, x := range u {
			raw[2*k], raw[2*k+1] = byte(x>>8), byte(x)
		}
		sb.WriteString("&" + mutf7.EncodeToString(raw) + "-")
		run = run[:0]
	}
	for _, r := range s {
		if r >= 0x20 && r <= 0x7e {
			flush()
			if r == '&' {
				sb.WriteString("&-")
			} else {
				sb.WriteRune(r)
			}
			continue
		}
		run = append(run, r)
	}
	flush()
	return sb.String()
}
