package ollama

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dperedo/bosun/internal/llm"
)

func TestNew_UsesV1AndName(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, `{"model":"llama","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	p, err := New(Config{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "ollama" {
		t.Fatalf("name %q", p.Name())
	}
	resp, err := p.Complete(context.Background(), llm.Request{Model: "llama", Messages: []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")}})
	if err != nil || resp.Message.Text() != "ok" || path != "/v1/chat/completions" {
		t.Fatalf("resp=%+v err=%v path=%s", resp, err, path)
	}
}
