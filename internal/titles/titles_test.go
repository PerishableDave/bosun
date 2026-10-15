package titles

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/llm/fake"
)

func TestQuick(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"  hi\n there ":           "hi there",
		"What's the weather like": "What's the weather like",
		"Plan a three day trip to Lisbon in October with kids please": "Plan a three day trip to Lisbon in…",
		strings.Repeat("x", 60):    strings.Repeat("x", 40) + "…",
		"bell\x07 and ​zero width": "bell and zero width",
	}
	for in, want := range cases {
		if got := Quick(in); got != want {
			t.Errorf("Quick(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Quick(strings.Repeat("é", 50)); len([]rune(got)) != 41 {
		t.Errorf("unicode quick = %q", got)
	}
}

func TestPlaceholder(t *testing.T) {
	for _, s := range []string{"", " ", "New chat", "new conversation", "Untitled"} {
		if !Placeholder(s) {
			t.Errorf("Placeholder(%q) = false", s)
		}
	}
	if Placeholder("Trip") {
		t.Error("Placeholder(Trip) = true")
	}
}

func TestParse(t *testing.T) {
	good := map[string]string{
		`{"title":"Lisbon trip planning"}`:                       "Lisbon trip planning",
		"```json\n{\"title\": \"\\\"Quoted title.\\\"\"}\n```":   "Quoted title",
		`{"title":"one two three four five six seven eight"}`:    "one two three four five six",
		`{"title":"line\nbreak\u0000 ok!"}`:                      "line break ok",
		`  {"title":"` + strings.Repeat("abcdefghij", 10) + `"}`: strings.Repeat("abcdefghij", 6),
	}
	for in, want := range good {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Lisbon trip", `{"title":""}`, `{"title":"..."}`, `{"name":"x"}`,
		`Sure! {"title":"x"}`, `{"title":"x"} {"title":"y"}`, `{"title":3}`} {
		if got, err := Parse(in); !errors.Is(err, ErrUnusable) {
			t.Errorf("Parse(%q) = %q, %v; want ErrUnusable", in, got, err)
		}
	}
}

func TestGenerator(t *testing.T) {
	p := fake.New("fake", fake.Text(`{"title":"Espresso machine advice"}`), fake.Text("I think: Coffee"))
	g := NewGenerator(p, "small", nil, nil)
	got, err := g.Title(context.Background(), 1, "which espresso machine?", "Try a Gaggia.")
	if err != nil || got != "Espresso machine advice" {
		t.Fatalf("Title = %q, %v", got, err)
	}
	req := p.Requests()[0]
	if req.Model != "small" || req.System != Prompt || !strings.Contains(req.Messages[0].Text(), "Try a Gaggia.") {
		t.Errorf("request = %+v", req)
	}
	if _, err := g.Title(context.Background(), 1, "a", "b"); !errors.Is(err, ErrUnusable) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := g.Title(context.Background(), 1, "a", "b"); err == nil {
		t.Error("exhausted script: no error")
	}
}
