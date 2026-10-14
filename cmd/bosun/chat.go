package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/api/events"
)

// streamClient has no overall timeout: a turn may run for minutes and waits
// on human approvals. Cancellation is via the request context.
var streamClient = &http.Client{}

// chatSession is one client conversation against the API in docs/API.md.
type chatSession struct {
	base, token string
	convID      string
	in          *bufio.Reader
	out, errOut io.Writer
	color       bool

	midLine bool // assistant text was printed without a trailing newline
}

func (c *chatSession) dim(s string) string {
	if !c.color {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

// useColor reports whether w is a terminal and NO_COLOR is unset.
func useColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func (c *chatSession) endLine() {
	if c.midLine {
		fmt.Fprintln(c.out)
		c.midLine = false
	}
}

func oneLineTrunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// create makes a new conversation.
func (c *chatSession) create(ctx context.Context, title string) error {
	var resp api.ConversationResponse
	if err := call(ctx, "POST", c.base+"/v1/conversations", c.token, api.CreateConversationRequest{Title: title}, &resp); err != nil {
		return err
	}
	c.convID = resp.ID
	return nil
}

// turn sends one user message and renders the resulting event stream.
func (c *chatSession) turn(ctx context.Context, content string) error {
	b, err := json.Marshal(api.PostMessageRequest{Content: content})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/conversations/"+url.PathEscape(c.convID)+"/messages", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := doHTTP(streamClient, req)
	if err != nil {
		return fmt.Errorf("send message: %w (is bosund running?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var evType string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if evType != "" || len(data) > 0 {
				done, err := c.handle(ctx, evType, strings.Join(data, "\n"))
				if done || err != nil {
					c.endLine()
					return err
				}
			}
			evType, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			evType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	c.endLine()
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("stream ended before the turn finished")
}

// errTurnFailed marks a turn that ended with an `error` event (already printed).
var errTurnFailed = errors.New("turn failed")

// handle processes one SSE event; done is true when the turn is over.
func (c *chatSession) handle(ctx context.Context, typ, data string) (done bool, err error) {
	switch typ {
	case events.TypeText:
		var d events.TextData
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return false, fmt.Errorf("bad %s event: %w", typ, err)
		}
		fmt.Fprint(c.out, d.Delta)
		c.midLine = !strings.HasSuffix(d.Delta, "\n")
	case events.TypeToolCall:
		var d events.ToolCallData
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return false, fmt.Errorf("bad %s event: %w", typ, err)
		}
		c.endLine()
		fmt.Fprintln(c.out, c.dim(fmt.Sprintf("⚙ %s(%s)", d.Name, oneLineTrunc(compactJSON(d.Args), 200))))
	case events.TypeToolResult:
		var d events.ToolResultData
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return false, fmt.Errorf("bad %s event: %w", typ, err)
		}
		c.endLine()
		fmt.Fprintln(c.out, c.dim("  → "+oneLineTrunc(d.Summary, 160)))
	case events.TypeApprovalRequired:
		var d events.ApprovalRequiredData
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return false, fmt.Errorf("bad %s event: %w", typ, err)
		}
		c.endLine()
		return false, c.approve(ctx, d)
	case events.TypeApprovalResolved:
		var d events.ApprovalResolvedData
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return false, fmt.Errorf("bad %s event: %w", typ, err)
		}
		c.endLine()
		fmt.Fprintln(c.out, c.dim(fmt.Sprintf("  approval %s: %s", d.ApprovalID, resolvedWord(d.Decision))))
	case events.TypeDone:
		return true, nil
	case events.TypeError:
		var d events.ErrorData
		_ = json.Unmarshal([]byte(data), &d)
		c.endLine()
		fmt.Fprintln(c.errOut, "error:", d.Message)
		return true, errTurnFailed
	}
	return false, nil
}

func resolvedWord(decision string) string {
	switch decision {
	case "approve":
		return "approved"
	case "deny":
		return "denied"
	}
	return decision
}

// approve prompts for an approval and posts the decision. Anything other than
// an explicit "y"/"yes" (including EOF) denies.
func (c *chatSession) approve(ctx context.Context, d events.ApprovalRequiredData) error {
	fmt.Fprintf(c.out, "Approve %s %s [%s risk] [y/N]? ", d.Tool, oneLineTrunc(compactJSON(d.Args), 300), d.Risk)
	line, rerr := c.in.ReadString('\n')
	if rerr != nil {
		fmt.Fprintln(c.out)
	}
	decision := "deny"
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		decision = "approve"
	}
	err := call(ctx, "POST", c.base+"/v1/approvals/"+url.PathEscape(d.ApprovalID), c.token, api.ResolveApprovalRequest{Decision: decision}, nil)
	if err != nil {
		// e.g. 409: answered elsewhere or expired. The stream still reports the outcome.
		fmt.Fprintf(c.errOut, "approval %s: %v\n", d.ApprovalID, err)
	}
	return nil
}

func apiError(resp *http.Response) error {
	var eb api.ErrorBody
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&eb) == nil && eb.Error.Message != "" {
		return fmt.Errorf("%s (HTTP %d)", eb.Error.Message, resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

// repl reads user lines until EOF or /exit.
func (c *chatSession) repl(ctx context.Context) error {
	for {
		fmt.Fprint(c.out, "> ")
		line, err := c.in.ReadString('\n')
		msg := strings.TrimSpace(line)
		if msg == "" && err != nil {
			fmt.Fprintln(c.out)
			return nil
		}
		if msg == "" {
			continue
		}
		if msg == "/exit" || msg == "/quit" {
			return nil
		}
		if terr := c.turn(ctx, msg); terr != nil && !errors.Is(terr, errTurnFailed) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(c.errOut, "error:", terr)
		}
		if err != nil {
			return nil
		}
	}
}

func newChatCmd(g *globals) *cobra.Command {
	var convID, title string
	cmd := &cobra.Command{
		Use:   "chat [message]",
		Short: "Chat with your assistant (interactive, or one message)",
		Long: "Without arguments, opens an interactive session (type /exit or press Ctrl-D to leave).\n" +
			"With a message argument, sends it as a single turn and exits.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := g.baseURL()
			if err != nil {
				return err
			}
			tok, err := g.readToken()
			if err != nil {
				return fmt.Errorf("%w (run `bosun login`)", err)
			}
			c := &chatSession{
				base: base, token: tok, convID: convID,
				in:  bufio.NewReader(cmd.InOrStdin()),
				out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr(),
			}
			c.color = useColor(c.out)
			ctx := cmd.Context()
			if c.convID == "" {
				if err := c.create(ctx, title); err != nil {
					return err
				}
			}
			if len(args) == 1 {
				if strings.TrimSpace(args[0]) == "" {
					return errors.New("message is empty")
				}
				if err := c.turn(ctx, args[0]); err != nil {
					if errors.Is(err, errTurnFailed) {
						return errors.New("the turn failed")
					}
					return err
				}
				return nil
			}
			fmt.Fprintln(c.out, c.dim("conversation "+c.convID+" (resume with --conversation "+c.convID+")"))
			return c.repl(ctx)
		},
	}
	cmd.Flags().StringVarP(&convID, "conversation", "c", "", "continue an existing conversation id")
	cmd.Flags().StringVar(&title, "title", "", "title for a new conversation")
	return cmd
}

// ---- approvals ----

type approvalInfo struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	Tool           string          `json:"tool"`
	Capability     string          `json:"capability"`
	Risk           string          `json:"risk"`
	Args           json.RawMessage `json:"args"`
	Status         string          `json:"status"`
	CreatedAt      time.Time       `json:"created_at"`
	ExpiresAt      time.Time       `json:"expires_at"`
}

func newApprovalsCmd(g *globals) *cobra.Command {
	list := func(cmd *cobra.Command, base, tok string) error {
		var resp struct {
			Approvals []approvalInfo `json:"approvals"`
		}
		if err := call(cmd.Context(), "GET", base+"/v1/approvals", tok, nil, &resp); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(resp.Approvals) == 0 {
			fmt.Fprintln(out, "No pending approvals.")
			return nil
		}
		for _, a := range resp.Approvals {
			fmt.Fprintf(out, "%s  %s  [%s]  expires %s\n    %s\n", a.ID, a.Tool, a.Risk,
				a.ExpiresAt.Local().Format("15:04:05"), oneLineTrunc(compactJSON(a.Args), 200))
		}
		return nil
	}
	setup := func(g *globals) (string, string, error) {
		base, err := g.baseURL()
		if err != nil {
			return "", "", err
		}
		tok, err := g.readToken()
		if err != nil {
			return "", "", fmt.Errorf("%w (run `bosun login`)", err)
		}
		return base, tok, nil
	}
	cmd := &cobra.Command{
		Use:   "approvals <id> approve|deny",
		Short: "List or answer pending approvals",
		Long:  "`bosun approvals list` shows pending approvals; `bosun approvals <id> approve|deny` answers one.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			decision := strings.ToLower(args[1])
			if decision != "approve" && decision != "deny" {
				return fmt.Errorf("decision must be approve or deny, got %q", args[1])
			}
			base, tok, err := setup(g)
			if err != nil {
				return err
			}
			var a approvalInfo
			err = call(cmd.Context(), "POST", base+"/v1/approvals/"+url.PathEscape(args[0]), tok, api.ResolveApprovalRequest{Decision: decision}, &a)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", args[0], a.Status)
			return nil
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List pending approvals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := setup(g)
			if err != nil {
				return err
			}
			return list(cmd, base, tok)
		},
	})
	return cmd
}
