package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// serverNameRe: lowercase segment usable in a capability. "__" is excluded
// because the agent encodes "." as "__" for model APIs, and a double
// underscore inside a segment could make two tool names collide.
var serverNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

const maxServerName = 32

// maxWireName is the longest tool name model APIs accept
// (^[a-zA-Z0-9_-]{1,64}$ after the agent replaces "." with "__").
const maxWireName = 64

func validServerName(s string) bool {
	return len(s) <= maxServerName && serverNameRe.MatchString(s) &&
		!strings.Contains(s, "__") && !strings.HasSuffix(s, "_")
}

// sanitizeToolName maps an MCP tool name to a capability segment matching
// [a-z0-9][a-z0-9_-]*: camelCase becomes snake_case, other characters become
// "_", runs of "_" collapse to one, and leading/trailing separators are
// trimmed.
func sanitizeToolName(s string) string {
	var b strings.Builder
	prevAlnumLower := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if prevAlnumLower {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			prevAlnumLower = false
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevAlnumLower = true
		case r == '-':
			b.WriteByte('-')
			prevAlnumLower = false
		default:
			b.WriteByte('_')
			prevAlnumLower = false
		}
	}
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	out = strings.Trim(out, "_-")
	if out == "" {
		out = "tool"
	}
	return out
}

// toolNamer assigns unique, length-bounded segments within one server.
type toolNamer struct {
	server string
	used   map[string]bool
}

func newToolNamer(server string) *toolNamer {
	return &toolNamer{server: server, used: map[string]bool{}}
}

// name returns the segment for original. The full Bosun name is
// "mcp.<server>.<segment>"; its wire form "mcp__<server>__<segment>" must fit
// maxWireName.
func (n *toolNamer) name(original string) string {
	budget := maxWireName - len("mcp__") - len(n.server) - len("__")
	base := sanitizeToolName(original)
	if len(base) > budget {
		sum := sha256.Sum256([]byte(original))
		base = strings.TrimRight(base[:budget-9], "_-") + "_" + hex.EncodeToString(sum[:4])
	}
	cand := base
	for i := 2; n.used[cand]; i++ {
		suffix := "_" + strconv.Itoa(i)
		b := base
		if len(b)+len(suffix) > budget {
			b = strings.TrimRight(b[:budget-len(suffix)], "_-")
		}
		cand = b + suffix
	}
	n.used[cand] = true
	return cand
}
