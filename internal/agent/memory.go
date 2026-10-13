package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dperedo/bosun/internal/memory"
)

// DefaultRecallLimit is the default number of relevance-ranked memories
// recalled per turn.
const DefaultRecallLimit = 8

// ExtractTimeout bounds one asynchronous memory extraction.
const ExtractTimeout = 30 * time.Second

// WithMemory enables per-user memory: each turn recalls the user's relevant
// memories into the system prompt, and after a turn that ends with done,
// extractor (if non-nil) runs asynchronously to propose new memories. Call
// Close (or Wait) to synchronize with pending extractions.
func WithMemory(store *memory.Store, extractor *memory.Extractor) Option {
	return func(a *Agent) { a.mem, a.extractor = store, extractor }
}

// WithRecallLimit sets how many relevance-ranked memories are recalled per
// turn (default DefaultRecallLimit; preferences are added on top).
func WithRecallLimit(n int) Option {
	return func(a *Agent) {
		if n > 0 {
			a.recallLimit = n
		}
	}
}

// WithBaseContext sets the context background work (memory extraction)
// derives from, usually the daemon's lifetime context. Default
// context.Background(). Close cancels it either way.
func WithBaseContext(ctx context.Context) Option {
	return func(a *Agent) {
		if ctx != nil {
			a.base = ctx
		}
	}
}

// Wait blocks until every background extraction started so far has
// finished. Turns that finish while Wait runs start theirs afterwards.
func (a *Agent) Wait() {
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	a.bg.Wait()
}

// Close cancels pending background extractions, waits for them to return,
// and stops new ones from starting. Turns may still run afterwards (without
// extraction). It is idempotent.
func (a *Agent) Close() {
	a.bgMu.Lock()
	a.bgClosed = true
	a.bgMu.Unlock()
	a.bgCancel()
	a.Wait()
}

// MemoryHeader and MemoryFooter delimit the recalled-memory section appended
// to SystemPrompt.
const (
	MemoryHeader = "=== What you remember about this user ==="
	MemoryFooter = "=== End of what you remember ==="
)

// systemPrompt returns SystemPrompt plus, when memory is enabled and
// anything is recalled, a delimited section listing the user's relevant
// memories. Recall failures are logged and the turn proceeds without them.
func (a *Agent) systemPrompt(ctx context.Context, userID int64, userText string) string {
	if a.mem == nil {
		return SystemPrompt
	}
	ms, err := a.mem.Recall(ctx, userID, userText, a.recallLimit)
	if err != nil {
		a.log.WarnContext(ctx, "agent: memory recall failed; continuing without memories", "user_id", userID, "err", err)
		return SystemPrompt
	}
	return SystemPrompt + FormatMemories(ms)
}

// FormatMemories renders recalled memories as the system-prompt section
// (empty string for none). Each memory is one bullet tagged with its kind;
// whitespace inside a memory is collapsed so it stays on one line.
func FormatMemories(ms []memory.Memory) string {
	if len(ms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n" + MemoryHeader + "\n")
	b.WriteString("These are things this user told you or asked you to remember in earlier conversations. " +
		"They are the user's own information, kept by Bosun, and may be out of date. " +
		"Use them when relevant; do not recite them unprompted.\n")
	for _, m := range ms {
		fmt.Fprintf(&b, "- (%s) %s\n", m.Kind, strings.Join(strings.Fields(m.Content), " "))
	}
	b.WriteString(MemoryFooter)
	return b.String()
}

// extractAsync starts memory extraction for a finished turn in the
// background, under a context derived from the base context (not the
// turn's) with ExtractTimeout. It is a no-op without an extractor or after
// Close.
func (a *Agent) extractAsync(t memory.Turn) {
	if a.extractor == nil {
		return
	}
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	if a.bgClosed {
		return
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		ctx, cancel := context.WithTimeout(a.bgCtx, ExtractTimeout)
		defer cancel()
		created, err := a.extractor.Extract(ctx, t)
		if err != nil {
			a.log.WarnContext(ctx, "agent: memory extraction failed", "user_id", t.UserID, "conversation_id", t.ConversationID, "err", err)
		}
		if len(created) > 0 {
			a.log.InfoContext(ctx, "agent: memories extracted", "user_id", t.UserID, "conversation_id", t.ConversationID, "count", len(created))
		}
	}()
}
