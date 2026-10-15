package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/store"
)

// maxExportToolOutput bounds each tool result shown in a Markdown export.
const maxExportToolOutput = 500

// handleExportConversation serves GET /v1/conversations/{id}/export as a
// download: ?format=markdown (default) or ?format=json (the same body as
// GET /v1/conversations/{id}).
func (s *Server) handleExportConversation(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" || format == "md" {
		format = "markdown"
	}
	if format != "markdown" && format != "json" {
		writeError(w, http.StatusBadRequest, "bad_request", `format must be "markdown" or "json"`)
		return
	}
	if s.convs == nil {
		notImplemented(w)
		return
	}
	u, _ := UserFromContext(r.Context())
	c, ok := s.ownConversation(w, r, u.ID)
	if !ok {
		return
	}
	d, err := s.conversationDetail(r, c)
	if err != nil {
		s.internalError(w, r, "list messages", err)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	if format == "json" {
		h.Set("Content-Disposition", `attachment; filename="`+exportName(c)+`.json"`)
		writeJSON(w, http.StatusOK, d)
		return
	}
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+exportName(c)+`.md"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(ExportMarkdown(c, d.Messages)))
}

// exportName is a safe file name (without extension) for c's export.
func exportName(c *store.Conversation) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(c.Title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 60 {
			break
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return c.ID
	}
	return name
}

// ExportMarkdown renders a conversation as Markdown: a heading, then each
// message under "You" / "Bosun" with its time; tool calls are listed with
// their arguments and (truncated) results.
func ExportMarkdown(c *store.Conversation, msgs []MessageResponse) string {
	var b strings.Builder
	title := c.Title
	if title == "" {
		title = "Conversation"
	}
	fmt.Fprintf(&b, "# %s\n\n", oneLine(title))
	fmt.Fprintf(&b, "_Exported from Bosun on %s · conversation `%s` · started %s_\n",
		time.Now().UTC().Format("2006-01-02 15:04 UTC"), c.ID, c.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
	lastRole := ""
	for _, m := range msgs {
		var blocks []llm.ContentBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		var body strings.Builder
		for _, bl := range blocks {
			switch bl.Type {
			case llm.BlockText:
				if t := strings.TrimSpace(bl.Text); t != "" {
					body.WriteString(t + "\n\n")
				}
			case llm.BlockToolUse:
				args := "{}"
				if len(bl.Input) > 0 {
					args = string(bl.Input)
				}
				fmt.Fprintf(&body, "> ⚙ Tool call `%s` `%s`\n\n", bl.Name, oneLine(args))
			case llm.BlockToolResult:
				label := "Result"
				if bl.IsError {
					label = "Error"
				}
				out := oneLine(bl.Content)
				if r := []rune(out); len(r) > maxExportToolOutput {
					out = string(r[:maxExportToolOutput]) + "…"
				}
				fmt.Fprintf(&body, "> %s: %s\n\n", label, out)
			}
		}
		if body.Len() == 0 {
			continue
		}
		who := "Bosun"
		if m.Role == string(llm.RoleUser) {
			who = "You"
		}
		// One heading per speaker run: tool calls and results stay under
		// Bosun's heading.
		if who != lastRole || who == "You" {
			fmt.Fprintf(&b, "\n## %s · %s\n\n", who, m.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
			lastRole = who
		}
		b.WriteString(body.String())
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
