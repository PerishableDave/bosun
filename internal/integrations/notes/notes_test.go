package notes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/integration"
)

func newNotes(t *testing.T, settings string, opts ...func(*Integration)) (*Integration, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Notes")
	if settings == "" {
		settings = "{}"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(settings), &m); err != nil {
		t.Fatal(err)
	}
	m["dir"] = dir
	b, _ := json.Marshal(m)
	n := New()
	for _, o := range opts {
		o(n)
	}
	if err := n.Init(context.Background(), integration.Config{Name: "notes", Settings: b}, nil); err != nil {
		t.Fatal(err)
	}
	return n, dir
}

func (n *Integration) tool(t *testing.T, name string) integration.Tool {
	t.Helper()
	for _, tl := range n.Tools() {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return integration.Tool{}
}

func call(t *testing.T, n *Integration, user int64, name, args string) (integration.Result, error) {
	t.Helper()
	ctx := integration.WithUserID(context.Background(), user)
	return n.tool(t, name).Handler(ctx, json.RawMessage(args))
}

func mustCall(t *testing.T, n *Integration, user int64, name, args string) integration.Result {
	t.Helper()
	r, err := call(t, n, user, name, args)
	if err != nil {
		t.Fatalf("%s %s: %v", name, args, err)
	}
	return r
}

func TestManifestAndRegistry(t *testing.T) {
	n := New()
	if m := n.Manifest(); m.Scope != integration.ScopePerUser || len(m.Capabilities) != 3 {
		t.Errorf("pre-init manifest = %+v", m)
	}
	n, _ = newNotes(t, "")
	reg := integration.NewRegistry()
	if err := reg.Register(n); err != nil {
		t.Fatal(err)
	}
	risks := map[string]integration.Risk{}
	for _, tl := range n.Tools() {
		risks[tl.Name] = tl.Risk
	}
	want := map[string]integration.Risk{"notes.list": "read", "notes.search": "read", "notes.read": "read",
		"notes.write": "write", "notes.append": "write", "notes.delete": "destructive"}
	for k, v := range want {
		if risks[k] != v {
			t.Errorf("%s risk = %q, want %q", k, risks[k], v)
		}
	}
	shared, _ := newNotes(t, `{"per_user":false}`)
	if shared.Manifest().Scope != integration.ScopeShared {
		t.Error("shared scope not reported")
	}
}

func TestInitErrors(t *testing.T) {
	for _, s := range []string{`{"dir":"relative/path"}`, `{"bogus":1}`, `{"per_user":"yes"}`} {
		if err := New().Init(context.Background(), integration.Config{Name: "notes", Settings: json.RawMessage(s)}, nil); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
	// Empty dir falls back to the data dir.
	dd := t.TempDir()
	n := New()
	if err := n.Init(context.Background(), integration.Config{Name: "notes", DataDir: dd}, nil); err != nil {
		t.Fatal(err)
	}
	mustCall(t, n, 1, "notes.write", `{"path":"a.md","content":"x"}`)
	if _, err := os.Stat(filepath.Join(dd, "u1", "a.md")); err != nil {
		t.Error(err)
	}
}

func TestWriteReadListAppendDelete(t *testing.T) {
	n, dir := newNotes(t, "")
	r := mustCall(t, n, 1, "notes.write", `{"path":"journal/2026/today.md","content":"# Today\nbought milk\n"}`)
	if !strings.Contains(r.Content, "Created journal/2026/today.md") || r.Untrusted {
		t.Errorf("write = %+v", r)
	}
	fi, err := os.Stat(filepath.Join(dir, "u1", "journal", "2026", "today.md"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	if r := mustCall(t, n, 1, "notes.write", `{"path":"journal/2026/today.md","content":"# Today\n"}`); !strings.Contains(r.Content, "Updated") {
		t.Errorf("overwrite = %q", r.Content)
	}
	mustCall(t, n, 1, "notes.append", `{"path":"journal/2026/today.md","content":"- call mum"}`)
	mustCall(t, n, 1, "notes.append", `{"path":"journal/2026/today.md","content":"- water plants\n"}`)
	mustCall(t, n, 1, "notes.append", `{"path":"new.txt","content":"first"}`)
	r = mustCall(t, n, 1, "notes.read", `{"path":"journal/2026/today.md"}`)
	if r.Content != "# Today\n- call mum\n- water plants\n" || r.Untrusted {
		t.Errorf("read = %q", r.Content)
	}
	if r := mustCall(t, n, 1, "notes.read", `{"path":"new.txt"}`); r.Content != "first" {
		t.Errorf("append-created = %q", r.Content)
	}

	if err := os.WriteFile(filepath.Join(dir, "u1", "image.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "u1", ".obsidian"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "u1", ".obsidian", "x.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = mustCall(t, n, 1, "notes.list", `{}`)
	entries := r.Data.([]Entry)
	if len(entries) != 2 || entries[0].Path != "journal/2026/today.md" || entries[1].Path != "new.txt" || entries[0].Modified.IsZero() {
		t.Errorf("list = %+v\n%s", entries, r.Content)
	}
	if r := mustCall(t, n, 1, "notes.list", `{"dir":"journal"}`); len(r.Data.([]Entry)) != 1 {
		t.Errorf("subdir list = %s", r.Content)
	}
	if _, err := call(t, n, 1, "notes.list", `{"dir":"missing"}`); err == nil {
		t.Error("missing dir listed")
	}

	mustCall(t, n, 1, "notes.delete", `{"path":"new.txt"}`)
	if _, err := call(t, n, 1, "notes.read", `{"path":"new.txt"}`); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("read deleted = %v", err)
	}
	if _, err := call(t, n, 1, "notes.delete", `{"path":"journal"}`); err == nil {
		t.Error("deleted a folder")
	}
}

func TestReadCap(t *testing.T) {
	n, dir := newNotes(t, "")
	mustCall(t, n, 1, "notes.write", `{"path":"big.md","content":"x"}`)
	if err := os.WriteFile(filepath.Join(dir, "u1", "big.md"), []byte(strings.Repeat("a", MaxReadBytes+10)), 0o600); err != nil {
		t.Fatal(err)
	}
	r := mustCall(t, n, 1, "notes.read", `{"path":"big.md"}`)
	if !strings.HasPrefix(r.Content, strings.Repeat("a", MaxReadBytes)) || !strings.Contains(r.Content, "[truncated") ||
		len(r.Content) > MaxReadBytes+100 {
		t.Errorf("read len = %d", len(r.Content))
	}
}

func TestSearch(t *testing.T) {
	n, _ := newNotes(t, "")
	mustCall(t, n, 1, "notes.write", `{"path":"a.md","content":"line one\nThe Quick fox\nline three\nline four"}`)
	mustCall(t, n, 1, "notes.write", `{"path":"sub/b.txt","content":"nothing\nquick again"}`)
	r := mustCall(t, n, 1, "notes.search", `{"query":"QUICK"}`)
	hits := r.Data.([]Match)
	if len(hits) != 2 || hits[0].Path != "a.md" || hits[0].Line != 2 || hits[0].StartLine != 1 ||
		strings.Join(hits[0].Context, "|") != "line one|The Quick fox|line three" || hits[1].Path != "sub/b.txt" {
		t.Errorf("hits = %+v", hits)
	}
	if !strings.Contains(r.Content, "> 2: The Quick fox") {
		t.Errorf("content = %s", r.Content)
	}
	r = mustCall(t, n, 1, "notes.search", `{"query":"^line (one|four)$","regex":true,"context":0}`)
	if hits := r.Data.([]Match); len(hits) != 2 || len(hits[0].Context) != 1 || hits[1].Line != 4 {
		t.Errorf("regex hits = %+v", hits)
	}
	if r := mustCall(t, n, 1, "notes.search", `{"query":"quick","dir":"sub"}`); len(r.Data.([]Match)) != 1 {
		t.Errorf("subdir search = %s", r.Content)
	}
	if r := mustCall(t, n, 1, "notes.search", `{"query":"line","max_results":2}`); len(r.Data.([]Match)) != 2 || !strings.Contains(r.Content, "stopped after 2") {
		t.Errorf("max_results = %s", r.Content)
	}
	if r := mustCall(t, n, 1, "notes.search", `{"query":"zebra"}`); len(r.Data.([]Match)) != 0 {
		t.Errorf("no match = %s", r.Content)
	}
	if _, err := call(t, n, 1, "notes.search", `{"query":"(","regex":true}`); err == nil {
		t.Error("bad regex accepted")
	}
}

func TestPathSafety(t *testing.T) {
	n, dir := newNotes(t, "")
	mustCall(t, n, 1, "notes.write", `{"path":"ok.md","content":"fine"}`)
	outside := filepath.Join(filepath.Dir(dir), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(dir, "u1")
	// Symlinks pointing outside the root.
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(userDir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(userDir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the root to a non-note file.
	if err := os.WriteFile(filepath.Join(userDir, "key.pem"), []byte("KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(userDir, "key.pem"), filepath.Join(userDir, "key.md")); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the root to a note is fine.
	if err := os.Symlink(filepath.Join(userDir, "ok.md"), filepath.Join(userDir, "alias.md")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ tool, args string }{
		{"notes.read", `{"path":"../../outside/secret.md"}`},
		{"notes.read", `{"path":"a/../../x.md"}`},
		{"notes.read", `{"path":"` + filepath.Join(outside, "secret.md") + `"}`},
		{"notes.read", `{"path":"/etc/hosts.md"}`},
		{"notes.read", `{"path":"link.md"}`},
		{"notes.read", `{"path":"linkdir/secret.md"}`},
		{"notes.read", `{"path":"key.md"}`},
		{"notes.read", `{"path":"key.pem"}`},
		{"notes.read", `{"path":".hidden.md"}`},
		{"notes.read", `{"path":"a\\..\\..\\x.md"}`},
		{"notes.write", `{"path":"../escape.md","content":"x"}`},
		{"notes.write", `{"path":"linkdir/new.md","content":"x"}`},
		{"notes.write", `{"path":"linkdir/deeper/new.md","content":"x"}`},
		{"notes.write", `{"path":"link.md","content":"overwrite"}`},
		{"notes.write", `{"path":"script.sh","content":"x"}`},
		{"notes.write", `{"path":"noext","content":"x"}`},
		{"notes.write", `{"path":".git/config.md","content":"x"}`},
		{"notes.append", `{"path":"link.md","content":"x"}`},
		{"notes.delete", `{"path":"link.md"}`},
		{"notes.delete", `{"path":"key.pem"}`},
		{"notes.list", `{"dir":"linkdir"}`},
		{"notes.list", `{"dir":".."}`},
		{"notes.search", `{"query":"SECRET","dir":"linkdir"}`},
	} {
		if r, err := call(t, n, 1, tc.tool, tc.args); err == nil {
			t.Errorf("%s %s accepted: %q", tc.tool, tc.args, r.Content)
		} else if strings.Contains(err.Error(), "TestPathSafety") && !strings.Contains(tc.args, "TestPathSafety") {
			t.Errorf("%s %s: error leaks the absolute path: %v", tc.tool, tc.args, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.md")); string(b) != "SECRET" {
		t.Error("outside file modified")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.md")); !errors.Is(err, os.ErrNotExist) {
		t.Error("file created outside the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "deeper")); !errors.Is(err, os.ErrNotExist) {
		t.Error("folder created outside the root")
	}
	// Search and list never follow symlinks out of the root.
	if r := mustCall(t, n, 1, "notes.search", `{"query":"SECRET"}`); len(r.Data.([]Match)) != 0 {
		t.Errorf("search followed a symlink: %s", r.Content)
	}
	if r := mustCall(t, n, 1, "notes.read", `{"path":"alias.md"}`); r.Content != "fine" {
		t.Errorf("in-root symlink = %q", r.Content)
	}
	mustCall(t, n, 1, "notes.write", `{"path":"./dot/ok.markdown","content":"x"}`)
	mustCall(t, n, 1, "notes.read", `{"path":"dot/ok.markdown"}`)
}

func TestPerUserIsolation(t *testing.T) {
	names := map[int64]string{1: "alice", 2: "bob"}
	n, dir := newNotes(t, "", func(n *Integration) {
		n.SetUserDirFunc(func(_ context.Context, id int64) (string, error) { return names[id], nil })
	})
	mustCall(t, n, 1, "notes.write", `{"path":"diary.md","content":"alice private"}`)
	mustCall(t, n, 2, "notes.write", `{"path":"todo.md","content":"bob stuff"}`)
	if _, err := os.Stat(filepath.Join(dir, "u1", "diary.md")); err != nil {
		t.Error(err)
	}
	if r := mustCall(t, n, 2, "notes.list", `{}`); len(r.Data.([]Entry)) != 1 || r.Data.([]Entry)[0].Path != "todo.md" {
		t.Errorf("bob sees %s", r.Content)
	}
	if r := mustCall(t, n, 2, "notes.search", `{"query":"private"}`); len(r.Data.([]Match)) != 0 {
		t.Errorf("bob found alice's note: %s", r.Content)
	}
	for _, args := range []string{`{"path":"../u1/diary.md"}`, `{"path":"diary.md"}`} {
		if _, err := call(t, n, 2, "notes.read", args); err == nil {
			t.Errorf("bob read %s", args)
		}
	}
	if _, err := n.tool(t, "notes.list").Handler(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("call without a user succeeded")
	}

	// Folders are keyed by user ID, not name.
	n2, dir2 := newNotes(t, "")
	mustCall(t, n2, 42, "notes.write", `{"path":"x.md","content":"x"}`)
	if _, err := os.Stat(filepath.Join(dir2, "u42", "x.md")); err != nil {
		t.Error(err)
	}
}

func TestLegacyFolderMigration(t *testing.T) {
	names := map[int64]string{1: "alice", 2: "../evil", 3: "u1"}
	n, dir := newNotes(t, "", func(n *Integration) {
		n.SetUserDirFunc(func(_ context.Context, id int64) (string, error) { return names[id], nil })
	})
	if err := os.MkdirAll(filepath.Join(dir, "alice"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alice", "old.md"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := mustCall(t, n, 1, "notes.read", `{"path":"old.md"}`); r.Content != "kept" {
		t.Errorf("migrated note = %q", r.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "alice")); !os.IsNotExist(err) {
		t.Errorf("legacy folder still present: %v", err)
	}
	// A new account reusing the name gets a fresh folder under its own ID.
	names[9] = "alice"
	if r := mustCall(t, n, 9, "notes.list", `{}`); len(r.Data.([]Entry)) != 0 {
		t.Errorf("new alice inherited notes: %s", r.Content)
	}
	// Unsafe names and names that look like ID folders are never migrated.
	mustCall(t, n, 2, "notes.list", `{}`)
	mustCall(t, n, 3, "notes.list", `{}`)
	if _, err := os.Stat(filepath.Join(dir, "u1", "old.md")); err != nil {
		t.Errorf("u1 clobbered: %v", err)
	}
}

func TestUntrustedByScope(t *testing.T) {
	per, _ := newNotes(t, `{"per_user":true}`)
	shared, dir := newNotes(t, `{"per_user":false}`)
	for _, n := range []*Integration{per, shared} {
		mustCall(t, n, 1, "notes.write", `{"path":"a.md","content":"hello"}`)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.md")); err != nil {
		t.Errorf("shared notes not at the root: %v", err)
	}
	// In shared mode every user sees the same notes.
	if r := mustCall(t, shared, 2, "notes.read", `{"path":"a.md"}`); r.Content != "hello" {
		t.Errorf("shared read = %q", r.Content)
	}
	for _, tc := range []struct{ tool, args string }{
		{"notes.list", `{}`}, {"notes.search", `{"query":"hello"}`}, {"notes.read", `{"path":"a.md"}`},
	} {
		if r := mustCall(t, per, 1, tc.tool, tc.args); r.Untrusted {
			t.Errorf("per-user %s untrusted", tc.tool)
		}
		if r := mustCall(t, shared, 1, tc.tool, tc.args); !r.Untrusted {
			t.Errorf("shared %s trusted", tc.tool)
		}
	}
}
