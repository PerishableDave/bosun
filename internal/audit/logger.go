package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

// Store is the subset of *store.Store the logger needs.
type Store interface {
	AppendAudit(ctx context.Context, e store.AuditEntry) (*store.AuditEntry, error)
}

var _ Store = (*store.Store)(nil)

// Options configure a StoreLogger.
type Options struct {
	// LogContent mirrors audit.log_content. When false (the default), every
	// string value in Args is replaced by a "<redacted N chars>" marker
	// (keys, numbers and booleans are kept), and in Detail the values under
	// content-bearing keys (see ContentKeys) are redacted the same way, so
	// metadata such as provider, model and token counts is still recorded.
	LogContent bool
}

// ContentKeys are the Detail object keys whose values are treated as content
// and redacted when LogContent is false.
var ContentKeys = map[string]bool{
	"content": true, "messages": true, "prompt": true, "system": true, "response": true,
	"text": true, "input": true, "output": true, "body": true, "args": true, "result": true,
}

// StoreLogger records entries in the append-only audit_log table.
type StoreLogger struct {
	st         Store
	logContent bool
}

var _ Logger = (*StoreLogger)(nil)

// NewStoreLogger returns a Logger backed by st.
func NewStoreLogger(st Store, opts Options) *StoreLogger {
	return &StoreLogger{st: st, logContent: opts.LogContent}
}

// Record implements Logger.
func (l *StoreLogger) Record(ctx context.Context, e Entry) error {
	if e.Kind == "" {
		return fmt.Errorf("audit: kind is required")
	}
	args, detail := e.Args, e.Detail
	if !l.logContent {
		args = RedactStrings(args)
		detail = redactKeys(detail)
	} else {
		args, detail = ensureJSON(args), ensureJSON(detail)
	}
	_, err := l.st.AppendAudit(ctx, store.AuditEntry{
		Time: e.Time, UserID: e.UserID, Kind: string(e.Kind), Integration: e.Integration, Tool: e.Tool,
		Capability: string(e.Capability), Risk: string(e.Risk), Decision: e.Decision, ApprovalID: e.ApprovalID,
		ApproverID: e.ApproverID, Args: args, ResultSummary: e.ResultSummary, Error: e.Error, Notes: e.Notes,
		Detail: detail,
	})
	if err != nil {
		return fmt.Errorf("audit: record %s: %w", e.Kind, err)
	}
	return nil
}

func redactedMarker(s string) string {
	return fmt.Sprintf("<redacted %d chars>", utf8.RuneCountInString(s))
}

// marshal encodes v without HTML escaping (so markers read "<redacted ...>"
// in the database rather than "\u003credacted ...").
func marshal(v any) (json.RawMessage, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(strings.TrimSuffix(b.String(), "\n")), nil
}

// ensureJSON returns raw unchanged if it is valid JSON (or empty), else the
// raw text encoded as a JSON string so the column always holds JSON.
func ensureJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || json.Valid(raw) {
		return raw
	}
	b, _ := json.Marshal(string(raw))
	return b
}

// RedactStrings replaces every string value in a JSON document with a
// "<redacted N chars>" marker, keeping structure, keys, numbers and booleans.
// Input that is not valid JSON is replaced by a single marker.
func RedactStrings(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		b, _ := marshal(redactedMarker(string(raw)))
		return b
	}
	b, err := marshal(redactAll(v))
	if err != nil {
		b, _ = marshal(redactedMarker(string(raw)))
	}
	return b
}

func redactAll(v any) any {
	switch x := v.(type) {
	case string:
		return redactedMarker(x)
	case []any:
		for i := range x {
			x[i] = redactAll(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = redactAll(x[k])
		}
		return x
	default:
		return v
	}
}

func redactKeys(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		b, _ := marshal(redactedMarker(string(raw)))
		return b
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if ContentKeys[strings.ToLower(k)] {
					x[k] = redactAll(val)
				} else {
					x[k] = walk(val)
				}
			}
			return x
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		default:
			return v
		}
	}
	b, err := marshal(walk(v))
	if err != nil {
		b, _ = marshal(redactedMarker(string(raw)))
	}
	return b
}

// SummaryLimit is the maximum length, in runes, of Summarize's output.
const SummaryLimit = 200

// Summarize returns a short single-line summary of a tool result for
// Entry.ResultSummary: whitespace is collapsed and the text is truncated to
// SummaryLimit runes (ending in "…" when cut). Untrusted results are prefixed
// with "[untrusted] ".
func Summarize(r integration.Result) string {
	s := strings.Join(strings.Fields(r.Content), " ")
	if s == "" && r.Data != nil {
		s = "(structured data)"
	}
	if r.Untrusted {
		s = "[untrusted] " + s
	}
	if utf8.RuneCountInString(s) <= SummaryLimit {
		return s
	}
	rs := []rune(s)
	return string(rs[:SummaryLimit-1]) + "…"
}
