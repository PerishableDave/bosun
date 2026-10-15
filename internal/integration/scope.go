package integration

import (
	"net"
	"net/url"
	"strings"
)

// NormalizeScope turns the value of a tool's ScopeArg into the scope key
// policy rules are stored under. Scopes are sites: a bare host
// ("www.OpenTable.com"), a host with a port or path, or a full URL all
// normalize to the lowercase host without a leading "www." and without a
// trailing dot ("opentable.com"). Values that do not look like a host
// yield "" (unscoped).
func NormalizeScope(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil {
			return ""
		}
		v = u.Hostname()
	} else {
		if i := strings.IndexAny(v, "/?#"); i >= 0 {
			v = v[:i]
		}
		if h, port, err := net.SplitHostPort(v); err == nil && allDigits(port) {
			v = h
		}
		v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	}
	v = strings.TrimSuffix(v, ".")
	v = strings.TrimPrefix(v, "www.")
	if v == "" || v == "www" || len(v) > 253 {
		return ""
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == ':':
		default:
			return ""
		}
	}
	if strings.Contains(v, ":") && net.ParseIP(v) == nil {
		return ""
	}
	return v
}

// ScopeCandidates returns the scopes a rule may be stored under that cover
// scope, most specific first: the scope itself, then each parent domain
// with at least two labels ("a.b.example.com" → "a.b.example.com",
// "b.example.com", "example.com"). IP addresses have no parents. A rule for
// "opentable.com" therefore also covers "m.opentable.com".
func ScopeCandidates(scope string) []string {
	if scope == "" {
		return nil
	}
	out := []string{scope}
	if net.ParseIP(scope) != nil {
		return out
	}
	labels := strings.Split(scope, ".")
	for i := 1; len(labels)-i >= 2; i++ {
		out = append(out, strings.Join(labels[i:], "."))
	}
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
