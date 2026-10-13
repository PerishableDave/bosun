package telegram

import (
	"strings"
	"unicode/utf8"
)

// MaxMessageLen is the longest text Bosun puts in one Telegram message, in
// UTF-16 code units (Telegram's limit is 4096; the margin absorbs headers).
const MaxMessageLen = 4000

// u16len returns the length of s in UTF-16 code units, the unit Telegram
// uses for entity offsets.
func u16len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Render turns the assistant's Markdown-flavoured text into plain text plus
// entities. Only code is translated: ``` fenced blocks become "pre"
// entities (an unclosed fence runs to the end, so partial streamed text
// renders sensibly) and `inline code` becomes "code". Everything else is
// sent verbatim, so there is nothing to escape and nothing a model (or text
// quoted from an email) can use to inject markup.
func Render(md string) (string, []MessageEntity) {
	var out strings.Builder
	var ents []MessageEntity
	off := 0 // UTF-16 offset of the end of out
	write := func(s string) {
		out.WriteString(s)
		off += u16len(s)
	}
	lines := strings.SplitAfter(md, "\n")
	inCode := false
	var lang string
	codeStart := 0
	closeCode := func() {
		// Exclude the trailing newline from the entity.
		end := off
		if strings.HasSuffix(out.String(), "\n") {
			end--
		}
		if end > codeStart {
			ents = append(ents, MessageEntity{Type: "pre", Offset: codeStart, Length: end - codeStart, Language: lang})
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inCode {
				inCode = true
				lang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				if strings.ContainsAny(lang, " `") {
					lang = ""
				}
				codeStart = off
			} else {
				closeCode()
				inCode = false
			}
			continue
		}
		if inCode {
			write(line)
			continue
		}
		renderInline(line, write, &ents, func() int { return off })
	}
	if inCode {
		closeCode()
	}
	return out.String(), ents
}

// renderInline writes one non-code line, turning `spans` into code entities.
func renderInline(line string, write func(string), ents *[]MessageEntity, off func() int) {
	for {
		i := strings.IndexByte(line, '`')
		if i < 0 {
			write(line)
			return
		}
		j := strings.IndexByte(line[i+1:], '`')
		if j <= 0 || strings.Contains(line[i+1:i+1+j], "\n") {
			// No closing backtick (or an empty span): keep it literal.
			write(line[:i+1])
			line = line[i+1:]
			continue
		}
		write(line[:i])
		code := line[i+1 : i+1+j]
		start := off()
		write(code)
		*ents = append(*ents, MessageEntity{Type: "code", Offset: start, Length: off() - start})
		line = line[i+2+j:]
	}
}

// splitText cuts s into pieces of at most max UTF-16 units, preferring to
// break after a newline in the second half of a piece.
func splitText(s string, max int) []string {
	var out []string
	for u16len(s) > max {
		cut := cutPoint(s, max)
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" || len(out) == 0 {
		out = append(out, s)
	}
	return out
}

// cutPoint returns a byte index at most max UTF-16 units into s (s must be
// longer than that).
func cutPoint(s string, max int) int {
	units, limit := 0, 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > max {
			limit = i
			break
		}
		units += w
	}
	if limit == 0 {
		_, size := utf8.DecodeRuneInString(s)
		return size // max is smaller than one rune; make progress anyway
	}
	if nl := strings.LastIndexByte(s[:limit], '\n'); nl >= limit/2 {
		return nl + 1
	}
	return limit
}

// withHeader prefixes rendered body text with a bold header line.
func withHeader(header, text string, ents []MessageEntity) (string, []MessageEntity) {
	if header == "" {
		return text, ents
	}
	if strings.TrimSpace(text) == "" {
		return header, []MessageEntity{{Type: "bold", Offset: 0, Length: u16len(header)}}
	}
	prefix := header + "\n\n"
	shift := u16len(prefix)
	out := []MessageEntity{{Type: "bold", Offset: 0, Length: u16len(header)}}
	for _, e := range ents {
		e.Offset += shift
		out = append(out, e)
	}
	return prefix + text, out
}
