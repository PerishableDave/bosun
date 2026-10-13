package calendar

// iCalendar (RFC 5545) content lines: a small, lossless component tree.
// Property values are kept in their raw (escaped) form so that a parsed
// calendar re-serializes without disturbing properties Bosun does not
// understand (VALARM, X- properties, VTIMEZONE, ...).

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Limits on what the parser accepts.
const (
	maxICSBytes = 5 << 20
	maxDepth    = 8
)

// Param is one property parameter (NAME=v1,v2).
type Param struct {
	Name   string
	Values []string
}

// Property is one content line. Value is the raw value text (still
// escaped for TEXT values; see Text).
type Property struct {
	Name   string
	Params []Param
	Value  string
}

// Param returns the first value of the named parameter ("" if absent).
func (p *Property) Param(name string) string {
	for _, x := range p.Params {
		if strings.EqualFold(x.Name, name) && len(x.Values) > 0 {
			return x.Values[0]
		}
	}
	return ""
}

// SetParam replaces the named parameter.
func (p *Property) SetParam(name string, vals ...string) {
	name = strings.ToUpper(name)
	for i, x := range p.Params {
		if strings.EqualFold(x.Name, name) {
			p.Params[i].Values = vals
			return
		}
	}
	p.Params = append(p.Params, Param{Name: name, Values: vals})
}

// Text returns the value unescaped as a TEXT value.
func (p *Property) Text() string { return unescapeText(p.Value) }

// Component is a BEGIN/END block.
type Component struct {
	Name     string
	Props    []*Property
	Children []*Component
}

// Prop returns the first property with the name, or nil.
func (c *Component) Prop(name string) *Property {
	for _, p := range c.Props {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// PropsNamed returns every property with the name.
func (c *Component) PropsNamed(name string) []*Property {
	var out []*Property
	for _, p := range c.Props {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// Text returns the unescaped text of the first property with the name.
func (c *Component) Text(name string) string {
	if p := c.Prop(name); p != nil {
		return p.Text()
	}
	return ""
}

// Remove deletes every property with the name.
func (c *Component) Remove(name string) {
	out := c.Props[:0]
	for _, p := range c.Props {
		if p.Name != name {
			out = append(out, p)
		}
	}
	c.Props = out
}

// Set replaces every property with the name by a single one (in the
// position of the first, or appended).
func (c *Component) Set(p *Property) {
	for i, x := range c.Props {
		if x.Name == p.Name {
			c.Props[i] = p
			rest := c.Props[i+1:]
			kept := c.Props[:i+1]
			for _, y := range rest {
				if y.Name != p.Name {
					kept = append(kept, y)
				}
			}
			c.Props = kept
			return
		}
	}
	c.Props = append(c.Props, p)
}

// SetText sets a TEXT property (escaping it), or removes it when v is "".
func (c *Component) SetText(name, v string) {
	if v == "" {
		c.Remove(name)
		return
	}
	c.Set(&Property{Name: name, Value: escapeText(v)})
}

// ChildrenNamed returns direct children with the name.
func (c *Component) ChildrenNamed(name string) []*Component {
	var out []*Component
	for _, ch := range c.Children {
		if ch.Name == name {
			out = append(out, ch)
		}
	}
	return out
}

// unfold joins folded lines (CRLF or LF followed by a space or tab) and
// splits the result into content lines. Blank lines are dropped.
func unfold(data string) []string {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	raw := strings.Split(data, "\n")
	var out []string
	for _, l := range raw {
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += l[1:]
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// parseLine parses one unfolded content line.
func parseLine(line string) (*Property, error) {
	// name
	i := strings.IndexAny(line, ";:")
	if i <= 0 {
		return nil, fmt.Errorf("malformed content line %q", truncate(line, 60))
	}
	p := &Property{Name: strings.ToUpper(line[:i])}
	rest := line[i:]
	for len(rest) > 0 && rest[0] == ';' {
		rest = rest[1:]
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("malformed parameter in %q", truncate(line, 60))
		}
		param := Param{Name: strings.ToUpper(rest[:eq])}
		rest = rest[eq+1:]
		for {
			var v string
			if len(rest) > 0 && rest[0] == '"' {
				end := strings.IndexByte(rest[1:], '"')
				if end < 0 {
					return nil, fmt.Errorf("unterminated quoted parameter in %q", truncate(line, 60))
				}
				v = rest[1 : 1+end]
				rest = rest[end+2:]
			} else {
				end := strings.IndexAny(rest, ",;:")
				if end < 0 {
					return nil, fmt.Errorf("missing value in %q", truncate(line, 60))
				}
				v = rest[:end]
				rest = rest[end:]
			}
			param.Values = append(param.Values, decodeParamValue(v))
			if len(rest) > 0 && rest[0] == ',' {
				rest = rest[1:]
				continue
			}
			break
		}
		p.Params = append(p.Params, param)
	}
	if len(rest) == 0 || rest[0] != ':' {
		return nil, fmt.Errorf("missing value in %q", truncate(line, 60))
	}
	p.Value = rest[1:]
	return p, nil
}

// decodeParamValue applies RFC 6868 caret encoding (^n, ^', ^^).
func decodeParamValue(v string) string {
	if !strings.Contains(v, "^") {
		return v
	}
	var sb strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '^' && i+1 < len(v) {
			switch v[i+1] {
			case 'n':
				sb.WriteByte('\n')
				i++
				continue
			case '\'':
				sb.WriteByte('"')
				i++
				continue
			case '^':
				sb.WriteByte('^')
				i++
				continue
			}
		}
		sb.WriteByte(v[i])
	}
	return sb.String()
}

// ParseICS parses an iCalendar object and returns its top-level component
// (normally VCALENDAR).
func ParseICS(data string) (*Component, error) { return parseICS(data, false) }

// parseICS is ParseICS; lenient skips malformed content lines instead of
// failing (used for large subscribed feeds, where one bad line should not
// hide the whole calendar).
func parseICS(data string, lenient bool) (*Component, error) {
	if len(data) > maxICSBytes {
		return nil, errors.New("icalendar object too large")
	}
	lines := unfold(strings.TrimPrefix(data, "\uFEFF"))
	var stack []*Component
	var root *Component
	for _, l := range lines {
		p, err := parseLine(l)
		if err != nil {
			if lenient {
				continue
			}
			return nil, err
		}
		switch p.Name {
		case "BEGIN":
			if len(stack) >= maxDepth {
				return nil, errors.New("icalendar components nested too deeply")
			}
			c := &Component{Name: strings.ToUpper(p.Value)}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, c)
			} else if root == nil {
				root = c
			} else {
				return nil, errors.New("multiple top-level icalendar components")
			}
			stack = append(stack, c)
		case "END":
			if len(stack) == 0 || stack[len(stack)-1].Name != strings.ToUpper(p.Value) {
				return nil, fmt.Errorf("unexpected END:%s", p.Value)
			}
			stack = stack[:len(stack)-1]
		default:
			if len(stack) == 0 {
				return nil, fmt.Errorf("property %s outside a component", p.Name)
			}
			c := stack[len(stack)-1]
			c.Props = append(c.Props, p)
		}
	}
	if root == nil {
		return nil, errors.New("no icalendar component found")
	}
	if len(stack) != 0 {
		return nil, fmt.Errorf("unterminated %s component", stack[len(stack)-1].Name)
	}
	return root, nil
}

// Encode serializes the component with CRLF line endings and lines folded
// at 75 octets.
func (c *Component) Encode() string {
	var sb strings.Builder
	c.encode(&sb)
	return sb.String()
}

func (c *Component) encode(sb *strings.Builder) {
	writeFolded(sb, "BEGIN:"+c.Name)
	for _, p := range c.Props {
		writeFolded(sb, p.line())
	}
	for _, ch := range c.Children {
		ch.encode(sb)
	}
	writeFolded(sb, "END:"+c.Name)
}

func (p *Property) line() string {
	var sb strings.Builder
	sb.WriteString(p.Name)
	for _, x := range p.Params {
		sb.WriteByte(';')
		sb.WriteString(x.Name)
		sb.WriteByte('=')
		for i, v := range x.Values {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(encodeParamValue(v))
		}
	}
	sb.WriteByte(':')
	sb.WriteString(p.Value)
	return sb.String()
}

func encodeParamValue(v string) string {
	v = strings.NewReplacer("^", "^^", "\n", "^n", "\"", "^'").Replace(v)
	if strings.ContainsAny(v, ",;:") {
		return `"` + v + `"`
	}
	return v
}

// writeFolded writes one content line, folding it so that no physical line
// exceeds 75 octets and multi-byte UTF-8 sequences are never split.
func writeFolded(sb *strings.Builder, line string) {
	const max = 75
	first := true
	for len(line) > 0 {
		limit := max
		if !first {
			limit = max - 1 // the leading space counts
		}
		if len(line) <= limit {
			if !first {
				sb.WriteByte(' ')
			}
			sb.WriteString(line)
			break
		}
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		if !first {
			sb.WriteByte(' ')
		}
		sb.WriteString(line[:cut])
		sb.WriteString("\r\n")
		line = line[cut:]
		first = false
	}
	sb.WriteString("\r\n")
}

// escapeText escapes a TEXT value.
func escapeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`).Replace(s)
}

// unescapeText reverses escapeText (accepting \N as well as \n).
func unescapeText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n', 'N':
				sb.WriteByte('\n')
			case '\\', ';', ',', ':', '"':
				sb.WriteByte(s[i+1])
			default:
				sb.WriteByte('\\')
				sb.WriteByte(s[i+1])
			}
			i++
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// splitList splits a comma-separated multi-value (date lists etc.).
func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
