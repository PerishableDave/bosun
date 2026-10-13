package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/api"
)

func TestRoutinesCommands(t *testing.T) {
	runPollInterval = 5 * time.Millisecond
	var mu sync.Mutex
	var got []string
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, s)
	}
	routine := `{"id":4,"name":"brief","prompt":"Summarize.","trigger":{"cron":"0 7 * * 1-5"},"deliver":["web","cli"],"enabled":true,"next_run_at":"2026-10-13T07:00:00Z","created_at":"2026-10-12T22:00:00Z","updated_at":"2026-10-12T22:00:00Z"}`
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/routines", func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateRoutineRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		record(fmt.Sprintf("POST %s|%s|%s|%s|%s|%v|%v", req.Name, req.Prompt, req.Trigger.Cron, req.Trigger.Event, req.Model, req.Deliver, *req.Enabled))
		if req.Trigger.Cron == "bad" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"code":"bad_request","message":"trigger: cron: \"bad\" has 1 fields, want 5"}}`)
			return
		}
		w.WriteHeader(201)
		fmt.Fprint(w, routine)
	})
	mux.HandleFunc("GET /v1/routines", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"routines":[%s,{"id":5,"name":"mail","prompt":"x","trigger":{"event":"email.received"},"deliver":["web"],"enabled":false,"created_at":"2026-10-12T22:00:00Z","updated_at":"2026-10-12T22:00:00Z"}]}`, routine)
	})
	mux.HandleFunc("GET /v1/routines/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "4" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"routine not found"}}`)
			return
		}
		fmt.Fprint(w, routine)
	})
	mux.HandleFunc("PUT /v1/routines/{id}", func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		json.NewDecoder(r.Body).Decode(&raw)
		b, _ := json.Marshal(raw)
		record("PUT " + r.PathValue("id") + " " + string(b))
		fmt.Fprint(w, routine)
	})
	mux.HandleFunc("DELETE /v1/routines/{id}", func(w http.ResponseWriter, r *http.Request) {
		record("DELETE " + r.PathValue("id"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v1/routines/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		record("RUN " + r.PathValue("id"))
		w.WriteHeader(202)
		fmt.Fprint(w, `{"id":9,"routine_id":4,"status":"running","source":"manual","started_at":"2026-10-12T22:00:00Z"}`)
	})
	mux.HandleFunc("GET /v1/routines/{id}/runs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		status, extra := "running", ""
		switch {
		case n == 2:
			status = "waiting_approval"
		case n >= 3:
			status, extra = "done", `,"reply":"Good morning.","conversation_id":"conv_1","finished_at":"2026-10-12T22:01:00Z"`
		}
		fmt.Fprintf(w, `{"runs":[{"id":9,"routine_id":4,"status":%q,"source":"manual","started_at":"2026-10-12T22:00:00Z"%s},
			{"id":8,"routine_id":4,"status":"error","source":"schedule","error":"model not available: x","started_at":"2026-10-11T07:00:00Z"}]}`, status, extra)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bosun.yaml")
	if err := os.WriteFile(cfgPath, fmt.Appendf(nil, "daemon: { data_dir: %q }\n", dir), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cli-token"), []byte("bsn_tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		return runCLI(t, "", append([]string{"--config", cfgPath, "--server", srv.URL}, args...)...)
	}
	cli := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}

	if out := cli("routines", "add", "--name", "brief", "--cron", "0 7 * * 1-5", "--prompt", "Summarize.", "--model", "fake/small"); !strings.Contains(out, "Created routine 4") {
		t.Errorf("add output:\n%s", out)
	}
	cli("routines", "add", "--name", "mail", "--event", "email.received", "--prompt", "Triage.", "--deliver", "web", "--disabled")
	if out, err := run("routines", "add", "--name", "x", "--cron", "bad", "--prompt", "p"); err == nil || !strings.Contains(err.Error(), "has 1 fields") {
		t.Errorf("bad cron: %v\n%s", err, out)
	}
	if _, err := run("routines", "add", "--name", "x", "--prompt", "p"); err == nil {
		t.Error("add without trigger accepted")
	}
	if _, err := run("routines", "add", "--name", "x", "--prompt", "p", "--cron", "@daily", "--event", "a.b"); err == nil {
		t.Error("add with both triggers accepted")
	}
	if out := cli("routines", "list"); !strings.Contains(out, "cron 0 7 * * 1-5") || !strings.Contains(out, "event email.received") {
		t.Errorf("list output:\n%s", out)
	}
	if out := cli("routines", "show", "4"); !strings.Contains(out, "Summarize.") || !strings.Contains(out, "model not available") || !strings.Contains(out, "(default)") {
		t.Errorf("show output:\n%s", out)
	}
	if _, err := run("routines", "show", "7"); err == nil || !strings.Contains(err.Error(), "routine not found") {
		t.Errorf("show missing: %v", err)
	}
	if _, err := run("routines", "show", "x"); err == nil {
		t.Error("bad id accepted")
	}
	cli("routines", "edit", "4", "--cron", "@hourly", "--disable", "--model", "")
	if _, err := run("routines", "edit", "4"); err == nil {
		t.Error("edit with no flags accepted")
	}
	cli("routines", "remove", "4")
	if out := cli("routines", "run", "4"); !strings.Contains(out, "Started run 9") {
		t.Errorf("run output:\n%s", out)
	}
	out := cli("routines", "run", "4", "--wait")
	if !strings.Contains(out, "Waiting for approval") || !strings.Contains(out, "Status: done") || !strings.Contains(out, "Good morning.") {
		t.Errorf("run --wait output:\n%s", out)
	}
	if out := cli("routines", "runs", "4"); !strings.Contains(out, "Good morning.") || !strings.Contains(out, "schedule") {
		t.Errorf("runs output:\n%s", out)
	}

	want := []string{
		"POST brief|Summarize.|0 7 * * 1-5||fake/small|[]|true",
		"POST mail|Triage.||email.received||[web]|false",
		"POST x|p|bad|||[]|true",
		`PUT 4 {"enabled":false,"model":"","trigger":{"cron":"@hourly"}}`,
		"DELETE 4",
		"RUN 4",
		"RUN 4",
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
