package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/auth"
	"github.com/dperedo/bosun/internal/routines"
	"github.com/dperedo/bosun/internal/store"
)

// echoRunner finishes every run immediately with a canned reply.
type echoRunner struct{}

func (echoRunner) RunRoutine(_ context.Context, r routines.Routine, inv routines.Invocation) (routines.Run, error) {
	run := inv.Run
	run.Status, run.Reply = routines.StatusDone, "ran "+r.Name
	return run, nil
}

func setupRoutines(t *testing.T) (e *env, aliceTok, bobTok string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(st, auth.Options{Argon2: &auth.Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	tok := func(name string) string {
		if _, err := a.CreateUser(ctx, name, "longenough", auth.RoleMember); err != nil {
			t.Fatal(err)
		}
		s, err := a.LoginWithLabel(ctx, name, "longenough", "test")
		if err != nil {
			t.Fatal(err)
		}
		return s.Token
	}
	aliceTok, bobTok = tok("alice"), tok("bob")
	rs := routines.NewStore(st, time.UTC)
	sched := routines.New(rs, echoRunner{}, routines.Options{})
	t.Cleanup(func() { sched.Close(time.Second) })
	bus := events.NewBus()
	s, err := New(Options{Auth: a, Bus: bus, Version: "test", Routines: routines.Service{Store: rs, Scheduler: sched}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, auth: a, bus: bus}, aliceTok, bobTok
}

func TestRoutineEndpoints(t *testing.T) {
	e, alice, bob := setupRoutines(t)
	decode := func(t *testing.T, code, want int, body any, resp interface{ Decode(any) error }) {
		t.Helper()
		if code != want {
			t.Fatalf("status = %d, want %d", code, want)
		}
		if err := resp.Decode(body); err != nil {
			t.Fatal(err)
		}
	}

	resp := e.do(t, "POST", "/v1/routines", alice, `{"name":"brief","prompt":"Summarize my day.","trigger":{"cron":"0 7 * * 1-5"},"model":"fake/small"}`)
	var rt RoutineResponse
	decode(t, resp.StatusCode, 201, &rt, json.NewDecoder(resp.Body))
	if rt.ID == 0 || rt.Name != "brief" || rt.Trigger.Cron != "0 7 * * 1-5" || !rt.Enabled || rt.NextRunAt == nil ||
		rt.LastRunAt != nil || strings.Join(rt.Deliver, ",") != "web,cli" || rt.Model != "fake/small" {
		t.Fatalf("created = %+v", rt)
	}

	// Validation: cron parse errors are reported with the reason.
	resp = e.do(t, "POST", "/v1/routines", alice, `{"name":"bad","prompt":"x","trigger":{"cron":"0 25 * * *"}}`)
	var eb ErrorBody
	decode(t, resp.StatusCode, 400, &eb, json.NewDecoder(resp.Body))
	if !strings.Contains(eb.Error.Message, "hour") || !strings.Contains(eb.Error.Message, "out of range") {
		t.Errorf("cron error message = %q", eb.Error.Message)
	}
	for _, body := range []string{
		`{"name":"x","prompt":"x","trigger":{}}`,
		`{"name":"x","prompt":"x","trigger":{"cron":"@daily","event":"a.b"}}`,
		`{"name":"x","prompt":"x","trigger":{"event":"Not Valid"}}`,
		`{"name":"","prompt":"x","trigger":{"cron":"@daily"}}`,
		`{"name":"x","prompt":"","trigger":{"cron":"@daily"}}`,
		`{"name":"x","prompt":"x","trigger":{"cron":"@daily"},"deliver":["sms"]}`,
		`{"name":"x","prompt":"x","trigger":{"cron":"@daily"},"extra":1}`,
	} {
		if resp := e.do(t, "POST", "/v1/routines", alice, body); resp.StatusCode != 400 {
			t.Errorf("create %s = %d, want 400", body, resp.StatusCode)
		}
	}
	if resp := e.do(t, "POST", "/v1/routines", alice, `{"name":"brief","prompt":"x","trigger":{"cron":"@daily"}}`); resp.StatusCode != 409 {
		t.Errorf("duplicate name = %d, want 409", resp.StatusCode)
	}
	resp = e.do(t, "POST", "/v1/routines", alice, `{"name":"mail","prompt":"Triage.","trigger":{"event":"email.received"},"enabled":false}`)
	var ev RoutineResponse
	decode(t, resp.StatusCode, 201, &ev, json.NewDecoder(resp.Body))
	if ev.Enabled || ev.NextRunAt != nil || ev.Trigger.Event != "email.received" {
		t.Errorf("event routine = %+v", ev)
	}

	path := fmt.Sprintf("/v1/routines/%d", rt.ID)
	var list RoutineListResponse
	resp = e.do(t, "GET", "/v1/routines", alice, "")
	decode(t, resp.StatusCode, 200, &list, json.NewDecoder(resp.Body))
	if len(list.Routines) != 2 {
		t.Errorf("alice list = %+v", list)
	}
	resp = e.do(t, "GET", "/v1/routines", bob, "")
	decode(t, resp.StatusCode, 200, &list, json.NewDecoder(resp.Body))
	if list.Routines == nil || len(list.Routines) != 0 {
		t.Errorf("bob list = %+v", list)
	}

	// Another user's routine is 404 everywhere.
	for _, c := range []struct{ method, path, body string }{
		{"GET", path, ""}, {"PUT", path, `{"enabled":false}`}, {"DELETE", path, ""},
		{"POST", path + "/run", ""}, {"GET", path + "/runs", ""},
		{"GET", "/v1/routines/abc", ""}, {"GET", "/v1/routines/999", ""},
	} {
		if resp := e.do(t, c.method, c.path, bob, c.body); resp.StatusCode != 404 {
			t.Errorf("bob %s %s = %d, want 404", c.method, c.path, resp.StatusCode)
		}
	}

	// Partial update.
	resp = e.do(t, "PUT", path, alice, `{"trigger":{"cron":"@hourly"},"model":""}`)
	var upd RoutineResponse
	decode(t, resp.StatusCode, 200, &upd, json.NewDecoder(resp.Body))
	if upd.Trigger.Cron != "@hourly" || upd.Model != "" || upd.Prompt != "Summarize my day." || upd.Name != "brief" {
		t.Errorf("updated = %+v", upd)
	}
	if resp := e.do(t, "PUT", path, alice, `{"trigger":{"cron":"* * *"}}`); resp.StatusCode != 400 {
		t.Errorf("bad cron update = %d", resp.StatusCode)
	}
	if resp := e.do(t, "PUT", path, alice, `{"name":"mail"}`); resp.StatusCode != 409 {
		t.Errorf("rename onto existing = %d", resp.StatusCode)
	}

	// Run now, then see it in the history.
	resp = e.do(t, "POST", path+"/run", alice, "")
	var run RoutineRunResponse
	decode(t, resp.StatusCode, 202, &run, json.NewDecoder(resp.Body))
	if run.ID == 0 || run.RoutineID != rt.ID || run.Source != "manual" || run.Status != "running" {
		t.Fatalf("run = %+v", run)
	}
	var runs RoutineRunListResponse
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp = e.do(t, "GET", path+"/runs?limit=5", alice, "")
		decode(t, resp.StatusCode, 200, &runs, json.NewDecoder(resp.Body))
		if len(runs.Runs) == 1 && runs.Runs[0].Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runs = %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := runs.Runs[0]; r.ID != run.ID || r.Reply != "ran brief" || r.FinishedAt == nil {
		t.Errorf("finished run = %+v", r)
	}
	if resp := e.do(t, "GET", path+"/runs?limit=0", alice, ""); resp.StatusCode != 400 {
		t.Errorf("limit=0 = %d", resp.StatusCode)
	}

	if resp := e.do(t, "DELETE", path, alice, ""); resp.StatusCode != 204 {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", path, alice, ""); resp.StatusCode != 404 {
		t.Errorf("get deleted = %d", resp.StatusCode)
	}
}

func TestRoutineEndpointsNotImplemented(t *testing.T) {
	e := setup(t)
	tok := e.login(t)
	if resp := e.do(t, "GET", "/v1/routines", tok, ""); resp.StatusCode != 501 {
		t.Errorf("without Routines = %d, want 501", resp.StatusCode)
	}
}
