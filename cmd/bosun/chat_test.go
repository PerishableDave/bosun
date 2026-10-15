package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/perishabledave/bosun/internal/api"
)

func sse(w http.ResponseWriter, evs ...[2]string) {
	w.Header().Set("Content-Type", "text/event-stream")
	f := w.(http.Flusher)
	fmt.Fprint(w, ": connected\n\n")
	for _, e := range evs {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e[0], e[1])
		f.Flush()
	}
}

type fakeServer struct {
	*httptest.Server
	mu        sync.Mutex
	messages  []string
	decisions map[string]string
	always    map[string]bool
	auth      string
}

func newFake(t *testing.T, script func(f *fakeServer, w http.ResponseWriter, msg string)) *fakeServer {
	f := &fakeServer{decisions: map[string]string{}, always: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		f.auth = r.Header.Get("Authorization")
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"conv_1","title":"","created_at":"2026-10-12T22:00:00Z"}`)
	})
	mux.HandleFunc("POST /v1/conversations/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var req api.PostMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || r.PathValue("id") != "conv_1" {
			http.Error(w, "bad", 400)
			return
		}
		f.mu.Lock()
		f.messages = append(f.messages, req.Content)
		f.mu.Unlock()
		script(f, w, req.Content)
	})
	mux.HandleFunc("POST /v1/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req api.ResolveApprovalRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.decisions[r.PathValue("id")] = req.Decision
		f.always[r.PathValue("id")] = req.Always
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":%q,"status":"approved"}`, r.PathValue("id"))
	})
	mux.HandleFunc("GET /v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"approvals":[{"id":"appr_9","tool":"email.send","risk":"write","args":{"to":"a@b.c"},"status":"pending","expires_at":"2026-10-12T22:15:00Z"}]}`)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func newSession(f *fakeServer, stdin string) (*chatSession, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return &chatSession{base: f.URL, token: "bsn_tok", in: bufio.NewReader(strings.NewReader(stdin)), out: &out, errOut: &errOut}, &out, &errOut
}

func TestChatApprovalRoundTrip(t *testing.T) {
	f := newFake(t, func(f *fakeServer, w http.ResponseWriter, msg string) {
		sse(w,
			[2]string{"text", `{"delta":"Let me "}`},
			[2]string{"text", `{"delta":"look."}`},
			[2]string{"tool_call", `{"id":"toolu_1","name":"websearch.fetch","args":{"url":"https://example.com"}}`},
			[2]string{"approval_required", `{"approval_id":"appr_1","tool":"websearch.fetch","args":{"url":"https://example.com"},"risk":"read"}`},
			[2]string{"approval_resolved", `{"approval_id":"appr_1","decision":"approve"}`},
			[2]string{"tool_result", `{"id":"toolu_1","summary":"fetched 12 KB"}`},
			[2]string{"text", `{"delta":"Done: it says hi."}`},
			[2]string{"done", `{}`},
		)
	})
	c, out, errOut := newSession(f, "y\n")
	ctx := context.Background()
	if err := c.create(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if f.auth != "Bearer bsn_tok" {
		t.Errorf("auth = %q", f.auth)
	}
	if err := c.turn(ctx, "what is on example.com?"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"Let me look.\n",
		`⚙ websearch.fetch({"url":"https://example.com"})`,
		"Approve websearch.fetch {\"url\":\"https://example.com\"} [read risk] [y/N]? ",
		"approval appr_1: approved",
		"→ fetched 12 KB",
		"Done: it says hi.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Error("color escape without color")
	}
	if f.decisions["appr_1"] != "approve" {
		t.Errorf("decisions = %v", f.decisions)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr: %s", errOut)
	}
}

func TestChatApprovalDefaultsToDeny(t *testing.T) {
	f := newFake(t, func(f *fakeServer, w http.ResponseWriter, msg string) {
		sse(w,
			[2]string{"approval_required", `{"approval_id":"appr_2","tool":"email.send","args":{},"risk":"write"}`},
			[2]string{"done", `{}`})
	})
	for stdin, want := range map[string]string{"\n": "deny", "": "deny", "n\n": "deny", "maybe\n": "deny", "YES\n": "approve"} {
		c, _, _ := newSession(f, stdin)
		c.convID = "conv_1"
		if err := c.turn(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
		if got := f.decisions["appr_2"]; got != want {
			t.Errorf("stdin %q: decision %q, want %q", stdin, got, want)
		}
	}
}

func TestChatErrorEventAndHTTPError(t *testing.T) {
	f := newFake(t, func(f *fakeServer, w http.ResponseWriter, msg string) {
		if msg == "conflict" {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"error":{"code":"conflict","message":"a turn is already running"}}`)
			return
		}
		if msg == "cut" {
			sse(w, [2]string{"text", `{"delta":"partial"}`})
			return
		}
		sse(w, [2]string{"text", `{"delta":"oops "}`}, [2]string{"error", `{"message":"model request failed"}`})
	})
	c, out, errOut := newSession(f, "")
	c.convID = "conv_1"
	if err := c.turn(context.Background(), "boom"); err != errTurnFailed {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(errOut.String(), "error: model request failed") || !strings.HasSuffix(out.String(), "oops \n") {
		t.Errorf("out=%q err=%q", out, errOut)
	}
	if err := c.turn(context.Background(), "conflict"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("conflict err = %v", err)
	}
	if err := c.turn(context.Background(), "cut"); err == nil || !strings.Contains(err.Error(), "before the turn finished") {
		t.Errorf("cut err = %v", err)
	}
}

func TestChatREPL(t *testing.T) {
	f := newFake(t, func(f *fakeServer, w http.ResponseWriter, msg string) {
		sse(w, [2]string{"text", fmt.Sprintf(`{"delta":"echo %s"}`, msg)}, [2]string{"done", `{}`})
	})
	c, out, _ := newSession(f, "hello\n\nsecond\n/exit\nignored\n")
	c.convID = "conv_1"
	if err := c.repl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.messages, ",") != "hello,second" {
		t.Errorf("messages = %v", f.messages)
	}
	if !strings.Contains(out.String(), "echo hello\n") || !strings.Contains(out.String(), "echo second\n") {
		t.Errorf("out: %q", out)
	}
}

func TestChatCommandsEndToEnd(t *testing.T) {
	f := newFake(t, func(f *fakeServer, w http.ResponseWriter, msg string) {
		sse(w, [2]string{"text", `{"delta":"hi"}`}, [2]string{"done", `{}`})
	})
	dir := t.TempDir()
	cfgPath := dir + "/conf/bosun.yaml"
	dataDir := dir + "/data"
	if out, err := runCLI(t, "dave\nlongenough\nlongenough\n", "--config", cfgPath, "setup", "--data-dir", dataDir, "--listen", "127.0.0.1:0"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	tokPath := dataDir + "/cli-token"
	if err := writeFile(tokPath, "bsn_tok\n"); err != nil {
		t.Fatal(err)
	}
	args := func(a ...string) []string { return append([]string{"--config", cfgPath, "--server", f.URL}, a...) }

	out, err := runCLI(t, "", args("chat", "hello")...)
	if err != nil || out != "hi\n" {
		t.Errorf("chat one-shot: %v %q", err, out)
	}
	out, err = runCLI(t, "yo\n", args("chat", "-c", "conv_1")...)
	if err != nil || !strings.Contains(out, "> hi\n") {
		t.Errorf("chat repl: %v %q", err, out)
	}
	out, err = runCLI(t, "", args("approvals", "list")...)
	if err != nil || !strings.Contains(out, "appr_9  email.send  [write]") || !strings.Contains(out, `{"to":"a@b.c"}`) {
		t.Errorf("approvals list: %v %q", err, out)
	}
	out, err = runCLI(t, "", args("approvals", "appr_9", "deny")...)
	if err != nil || !strings.Contains(out, "appr_9:") || f.decisions["appr_9"] != "deny" {
		t.Errorf("approvals deny: %v %q %v", err, out, f.decisions)
	}
	if _, err = runCLI(t, "", args("approvals", "appr_9", "maybe")...); err == nil {
		t.Error("bad decision accepted")
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }

// TestChatApprovalAlwaysOnSite: a scoped, non-destructive approval offers
// "always on <site>"; a destructive one does not (and "a" then denies).
func TestChatApprovalAlwaysOnSite(t *testing.T) {
	for _, tc := range []struct {
		risk, prompt, decision string
		always                 bool
	}{
		{"read", "[read risk] [y]es / [a]lways on example.com / [N]o? ", "approve", true},
		{"destructive", "[destructive risk] [y/N]? ", "deny", false},
	} {
		f := newFake(t, func(f *fakeServer, w http.ResponseWriter, msg string) {
			sse(w,
				[2]string{"approval_required", `{"approval_id":"appr_3","tool":"browser.open","args":{"url":"https://example.com/"},"risk":"` +
					tc.risk + `","scope":"example.com"}`},
				[2]string{"done", `{}`})
		})
		c, out, _ := newSession(f, "a\n")
		c.convID = "conv_1"
		if err := c.turn(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tc.prompt) {
			t.Errorf("%s: prompt missing %q:\n%s", tc.risk, tc.prompt, out)
		}
		if got := f.decisions["appr_3"]; got != tc.decision {
			t.Errorf("%s: decision %q, want %q", tc.risk, got, tc.decision)
		}
		if f.always["appr_3"] != tc.always {
			t.Errorf("%s: always = %v", tc.risk, f.always["appr_3"])
		}
	}
}
