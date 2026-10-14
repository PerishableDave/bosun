// Package builtin holds Bosun's built-in Go hooks. They double as worked
// examples of the hook API.
package builtin

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/perishabledave/bosun/internal/llm"
)

// RedactSecretsName is the registry name of RedactSecrets.
const RedactSecretsName = "builtin.redact_secrets"

// secretPatterns match strings that look like credentials. Each is replaced
// by "[REDACTED:<label>]". The replacement contains no JSON metacharacters,
// so it is safe inside tool_use input JSON as well as text.
var secretPatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"anthropic-key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`)},
	{"github-token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{"slack-token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"hex-secret", regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)},
}

// Redact returns s with every secret-looking substring masked, and whether
// anything changed.
func Redact(s string) (string, bool) {
	out := s
	for _, p := range secretPatterns {
		out = p.re.ReplaceAllLiteralString(out, "[REDACTED:"+p.label+"]")
	}
	return out, out != s
}

// RedactSecrets is a pre_model_call hook (hooks.PreModelCall) that masks
// strings that look like API keys and tokens (Anthropic "sk-ant-", GitHub
// "ghp_" and friends, Slack "xoxb-", AWS "AKIA...", JWTs, and runs of 32 or
// more hex digits) in the system prompt and every message before the request
// leaves the machine: text blocks, tool_result content, and tool_use input.
//
// It only changes what is sent to the model; the conversation history and
// the audit log keep the original text. Hex runs of 32+ characters also match
// things like git commit hashes; that is the price of catching hex tokens.
func RedactSecrets(_ context.Context, req *llm.Request) error {
	req.System, _ = Redact(req.System)
	for i := range req.Messages {
		// RunPreModelCall hands hooks private copies of the content slices,
		// so assigning blocks in place does not touch the caller's history.
		for j := range req.Messages[i].Content {
			b := &req.Messages[i].Content[j]
			b.Text, _ = Redact(b.Text)
			b.Content, _ = Redact(b.Content)
			if len(b.Input) > 0 {
				if s, changed := Redact(string(b.Input)); changed && json.Valid([]byte(s)) {
					b.Input = json.RawMessage(s)
				}
			}
		}
	}
	return nil
}
