package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dperedo/bosun/internal/hooks"
	"github.com/dperedo/bosun/internal/llm"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"key sk-ant-api03-AbCdEf_123-xyz done":                  "key [REDACTED:anthropic-key] done",
		"tok ghp_" + strings.Repeat("a1B2", 9):                  "tok [REDACTED:github-token]",
		"pat github_pat_" + strings.Repeat("x", 30):             "pat [REDACTED:github-token]",
		"slack xoxb-1234567890-abcdefghij end":                  "slack [REDACTED:slack-token] end",
		"aws AKIAIOSFODNN7EXAMPLE.":                             "aws [REDACTED:aws-access-key].",
		"hex " + strings.Repeat("deadbeef", 4) + " x":           "hex [REDACTED:hex-secret] x",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefgh": "jwt [REDACTED:jwt]",
	}
	for in, want := range cases {
		if got, changed := Redact(in); got != want || !changed {
			t.Errorf("Redact(%q) = %q, %v; want %q", in, got, changed, want)
		}
	}
	for _, keep := range []string{
		"plain text with sk-ant and ghp_ prefixes only",
		"short hex deadbeef and AKIA lowercase akiaiosfodnn7example",
		"uuid 123e4567-e89b-12d3-a456-426614174000",
	} {
		if got, changed := Redact(keep); changed {
			t.Errorf("Redact(%q) changed it to %q", keep, got)
		}
	}
}

func TestRedactSecretsHook(t *testing.T) {
	secret := "sk-ant-api03-SECRETSECRET"
	input := json.RawMessage(`{"token":"` + secret + `","n":1}`)
	history := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "my key is "+secret),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolUseBlock("t1", "x__y", input)}},
		{Role: llm.RoleTool, Content: []llm.ContentBlock{llm.ToolResultBlock("t1", "echo "+secret, false)}},
	}
	req := llm.Request{System: "memory: " + secret, Messages: history}
	reg := hooks.NewRegistry()
	if err := reg.AddPreModelCall(RedactSecretsName, RedactSecrets); err != nil {
		t.Fatal(err)
	}
	if err := reg.RunPreModelCall(context.Background(), 1, &req); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req)
	if strings.Contains(string(b), "SECRETSECRET") {
		t.Fatalf("secret leaked: %s", b)
	}
	if !json.Valid(req.Messages[1].Content[0].Input) || !strings.Contains(string(req.Messages[1].Content[0].Input), `"n":1`) {
		t.Errorf("tool input = %s", req.Messages[1].Content[0].Input)
	}
	if req.Messages[1].Content[0].ID != "t1" || req.Messages[2].Content[0].ToolUseID != "t1" {
		t.Error("ids changed")
	}
	// The caller's history is untouched.
	if !strings.Contains(history[0].Content[0].Text, secret) || !strings.Contains(string(history[1].Content[0].Input), secret) ||
		!strings.Contains(history[2].Content[0].Content, secret) {
		t.Errorf("history modified: %+v", history)
	}
}
