package integration

import (
	"slices"
	"testing"
)

func TestNormalizeScope(t *testing.T) {
	for in, want := range map[string]string{
		"opentable.com":                          "opentable.com",
		"  WWW.OpenTable.com. ":                  "opentable.com",
		"https://www.opentable.com/r/x?y=1":      "opentable.com",
		"http://Example.COM:8080/":               "example.com",
		"example.com:443":                        "example.com",
		"example.com/path":                       "example.com",
		"127.0.0.1:8080":                         "127.0.0.1",
		"http://[::1]:80/":                       "::1",
		"":                                       "",
		"not a host":                             "",
		"javascript:alert(1)":                    "",
		"file:///etc/passwd":                     "",
		"www.":                                   "",
		"sub.www.example.com":                    "sub.www.example.com",
		"xn--bcher-kva.example":                  "xn--bcher-kva.example",
		"https://user:pass@shop.example.com/buy": "shop.example.com",
	} {
		if got := NormalizeScope(in); got != want {
			t.Errorf("NormalizeScope(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScopeCandidates(t *testing.T) {
	if got := ScopeCandidates("a.b.example.com"); !slices.Equal(got, []string{"a.b.example.com", "b.example.com", "example.com"}) {
		t.Errorf("got %v", got)
	}
	if got := ScopeCandidates("example.com"); !slices.Equal(got, []string{"example.com"}) {
		t.Errorf("got %v", got)
	}
	if got := ScopeCandidates("10.0.0.1"); !slices.Equal(got, []string{"10.0.0.1"}) {
		t.Errorf("got %v", got)
	}
	if got := ScopeCandidates(""); got != nil {
		t.Errorf("got %v", got)
	}
}
