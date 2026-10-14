package audit

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func lastEntry(t *testing.T, s *store.Store) *store.AuditEntry {
	t.Helper()
	es, err := s.ListAudit(context.Background(), store.AuditFilter{Limit: 1000})
	if err != nil || len(es) == 0 {
		t.Fatalf("ListAudit: %v (n=%d)", err, len(es))
	}
	return es[len(es)-1]
}

func TestRecordRedactsByDefault(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	l := NewStoreLogger(s, Options{})
	err := l.Record(ctx, Entry{
		UserID: 1, Kind: KindToolCall, Integration: "email", Tool: "email.send", Capability: "email.send",
		Risk: integration.RiskWrite, Decision: "allow", Notes: []string{"hook: ok"},
		Args:   json.RawMessage(`{"to":"a@b.example","body":"héllo","n":3,"cc":["x"],"urgent":true}`),
		Detail: json.RawMessage(`{"provider":"anthropic","model":"m","input_tokens":5,"messages":[{"text":"secret"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := lastEntry(t, s)
	var args map[string]any
	if err := json.Unmarshal(e.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args["to"] != "<redacted 11 chars>" || args["body"] != "<redacted 5 chars>" || args["n"] != float64(3) ||
		args["urgent"] != true || args["cc"].([]any)[0] != "<redacted 1 chars>" {
		t.Errorf("args = %s", e.Args)
	}
	d := string(e.Detail)
	if strings.Contains(d, "secret") || !strings.Contains(d, `"provider":"anthropic"`) || !strings.Contains(d, `"input_tokens":5`) {
		t.Errorf("detail = %s", d)
	}
	if e.Tool != "email.send" || e.Decision != "allow" || len(e.Notes) != 1 {
		t.Errorf("entry = %+v", e)
	}
}

func TestRecordLogContent(t *testing.T) {
	s := newStore(t)
	l := NewStoreLogger(s, Options{LogContent: true})
	args := `{"to":"a@b.example"}`
	if err := l.Record(context.Background(), Entry{Kind: KindToolCall, Args: json.RawMessage(args),
		Detail: json.RawMessage(`{"messages":"hi"}`)}); err != nil {
		t.Fatal(err)
	}
	e := lastEntry(t, s)
	if string(e.Args) != args || !strings.Contains(string(e.Detail), "hi") {
		t.Errorf("content not kept: %s %s", e.Args, e.Detail)
	}
	// Invalid JSON args are stored as a JSON string, not raw.
	if err := l.Record(context.Background(), Entry{Kind: KindToolCall, Args: json.RawMessage(`{not json`)}); err != nil {
		t.Fatal(err)
	}
	if e := lastEntry(t, s); !json.Valid(e.Args) {
		t.Errorf("invalid args stored raw: %s", e.Args)
	}
}

func TestRecordRequiresKind(t *testing.T) {
	l := NewStoreLogger(newStore(t), Options{})
	if err := l.Record(context.Background(), Entry{}); err == nil {
		t.Error("want error for empty kind")
	}
}

func TestRedactInvalidJSON(t *testing.T) {
	got := RedactStrings(json.RawMessage(`{oops`))
	var s string
	if err := json.Unmarshal(got, &s); err != nil || s != "<redacted 5 chars>" {
		t.Errorf("got %s", got)
	}
}

func TestSummarize(t *testing.T) {
	if got := Summarize(integration.Result{Content: "  hello\n\n world \t"}); got != "hello world" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 500)
	got := Summarize(integration.Result{Content: long})
	if utf8.RuneCountInString(got) != SummaryLimit || !strings.HasSuffix(got, "…") {
		t.Errorf("len %d", utf8.RuneCountInString(got))
	}
	if got := Summarize(integration.Result{Content: "x", Untrusted: true}); got != "[untrusted] x" {
		t.Errorf("got %q", got)
	}
	if got := Summarize(integration.Result{Data: map[string]int{"a": 1}}); got != "(structured data)" {
		t.Errorf("got %q", got)
	}
}
