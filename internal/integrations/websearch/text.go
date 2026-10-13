package websearch

import (
	"html"
	"strings"
	"unicode"
)

// blockTags break a line when opened or closed.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "ul": true, "ol": true, "tr": true,
	"table": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"section": true, "article": true, "header": true, "footer": true, "blockquote": true,
	"pre": true, "hr": true, "title": true, "main": true, "nav": true,
}

// skipTags have their whole content dropped.
var skipTags = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true}

// htmlToText is a small tag stripper: it drops script/style contents and
// comments, turns block tags into line breaks, decodes entities, and
// collapses whitespace.
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
			break
		}
		tag := s[i+1 : i+end]
		i += end + 1
		name := tagName(tag)
		if name == "" {
			sb.WriteByte('<') // stray "<": keep literally
			sb.WriteString(tag)
			sb.WriteByte('>')
			continue
		}
		if skipTags[name] && !strings.HasPrefix(tag, "/") {
			closeTag := "</" + name
			idx := indexFold(s[i:], closeTag)
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
		if blockTags[name] {
			sb.WriteByte('\n')
		} else {
			sb.WriteByte(' ')
		}
	}
	return dropBlankLines(collapseSpace(html.UnescapeString(sb.String())))
}

func tagName(tag string) string {
	tag = strings.TrimPrefix(tag, "/")
	n := 0
	for n < len(tag) && (tag[n] >= 'a' && tag[n] <= 'z' || tag[n] >= 'A' && tag[n] <= 'Z' || tag[n] >= '0' && tag[n] <= '9') {
		n++
	}
	if n == 0 || (len(tag) > 0 && !(tag[0] >= 'a' && tag[0] <= 'z' || tag[0] >= 'A' && tag[0] <= 'Z' || tag[0] == '!')) {
		if strings.HasPrefix(tag, "!") {
			return "!"
		}
		return ""
	}
	return strings.ToLower(tag[:n])
}

func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

// collapseSpace trims lines, collapses runs of spaces to one, and runs of
// blank lines to a single blank line.
func collapseSpace(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, l := range lines {
		l = strings.Join(strings.FieldsFunc(l, func(r rune) bool { return unicode.IsSpace(r) || r == 0xa0 }), " ")
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func dropBlankLines(s string) string {
	return strings.ReplaceAll(s, "\n\n", "\n")
}

// oneLine flattens s onto a single line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
