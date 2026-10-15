package files

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perishabledave/bosun/internal/integration"
)

type env struct {
	f       *Integration
	shared  string // writable root dir (resolved)
	media   string // read-only root dir
	outside string
}

func newEnv(t *testing.T, extra map[string]any) *env {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{shared: filepath.Join(base, "shared"), media: filepath.Join(base, "media"), outside: filepath.Join(base, "outside")}
	for _, d := range []string{e.shared, e.media, e.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := map[string]any{"roots": []map[string]any{
		{"path": e.shared, "name": "shared", "writable": true},
		{"path": e.media, "name": "media", "writable": false},
	}}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	e.f = New()
	if err := e.f.Init(context.Background(), integration.Config{Name: "files", Settings: b}, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

func put(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *env) call(t *testing.T, name, args string) (integration.Result, error) {
	t.Helper()
	for _, tl := range e.f.Tools() {
		if tl.Name == name {
			return tl.Handler(integration.WithUserID(context.Background(), 1), json.RawMessage(args))
		}
	}
	t.Fatalf("no tool %s", name)
	return integration.Result{}, nil
}

func (e *env) must(t *testing.T, name, args string) integration.Result {
	t.Helper()
	r, err := e.call(t, name, args)
	if err != nil {
		t.Fatalf("%s %s: %v", name, args, err)
	}
	return r
}

func (e *env) fail(t *testing.T, name, args, want string) {
	t.Helper()
	r, err := e.call(t, name, args)
	if err == nil {
		t.Fatalf("%s %s: expected error containing %q, got %q", name, args, want, r.Content)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s %s: error %q does not contain %q", name, args, err, want)
	}
	for _, p := range []string{e.shared, e.media, e.outside} {
		if strings.Contains(err.Error(), p) {
			t.Fatalf("error leaks host path: %v", err)
		}
	}
}

func TestManifestAndRegistry(t *testing.T) {
	f := New()
	reg := integration.NewRegistry()
	if err := reg.Register(f); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := f.Manifest()
	if m.Scope != integration.ScopeShared {
		t.Errorf("scope = %s", m.Scope)
	}
	risks := map[string]integration.Risk{}
	for _, tl := range f.Tools() {
		risks[tl.Name] = tl.Risk
	}
	want := map[string]integration.Risk{
		"files.roots": integration.RiskRead, "files.list": integration.RiskRead, "files.search": integration.RiskRead,
		"files.read": integration.RiskRead, "files.stat": integration.RiskRead, "files.write": integration.RiskWrite,
		"files.append": integration.RiskWrite, "files.move": integration.RiskWrite, "files.mkdir": integration.RiskWrite,
		"files.delete": integration.RiskDestructive,
	}
	for n, r := range want {
		if risks[n] != r {
			t.Errorf("%s risk = %q, want %q", n, risks[n], r)
		}
	}
}

func TestInitErrors(t *testing.T) {
	dir := t.TempDir()
	for name, cfg := range map[string]string{
		"no roots":     `{}`,
		"bad name":     `{"roots":[{"path":"` + dir + `","name":"Bad Name"}]}`,
		"dup name":     `{"roots":[{"path":"` + dir + `","name":"a"},{"path":"` + dir + `","name":"a"}]}`,
		"relative":     `{"roots":[{"path":"rel/dir","name":"a"}]}`,
		"missing":      `{"roots":[{"path":"` + dir + `/nope","name":"a"}]}`,
		"bad max_read": `{"roots":[{"path":"` + dir + `","name":"a"}],"max_read":"lots"}`,
		"unknown key":  `{"roots":[{"path":"` + dir + `","name":"a"}],"bogus":1}`,
	} {
		if err := New().Init(context.Background(), integration.Config{Name: "files", Settings: json.RawMessage(cfg)}, nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestHomeExpansionAndRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "Documents", "Shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := New()
	cfg := `{"roots":[{"path":"~/Documents/Shared","name":"shared","writable":true},{"path":"` + home + `","name":"home"}],"max_read":"1KiB","extensions":["md","TXT"]}`
	if err := f.Init(context.Background(), integration.Config{Name: "files", Settings: json.RawMessage(cfg)}, nil); err != nil {
		t.Fatal(err)
	}
	if f.maxRead != 1024 {
		t.Errorf("maxRead = %d", f.maxRead)
	}
	r, err := f.rootsTool(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"shared/ (writable)", "home/ (read-only)", ".md .txt"} {
		if !strings.Contains(r.Content, w) {
			t.Errorf("roots output missing %q:\n%s", w, r.Content)
		}
	}
	if strings.Contains(r.Content, home) {
		t.Errorf("roots leaks host path:\n%s", r.Content)
	}
	if !r.Untrusted {
		t.Error("roots must be untrusted")
	}
	if got := f.byName["shared"].real; !strings.HasSuffix(got, filepath.Join("Documents", "Shared")) {
		t.Errorf("shared root = %q", got)
	}
}

func TestParsePath(t *testing.T) {
	e := newEnv(t, nil)
	hid := newEnv(t, map[string]any{"include_hidden": true})
	long := strings.Repeat("a", 256)
	cases := []struct {
		in      string
		f       *Integration
		wantRel string
		wantErr string
	}{
		{"shared/a.md", e.f, "a.md", ""},
		{"shared", e.f, ".", ""},
		{"shared/", e.f, ".", ""},
		{"shared//sub/./a.md", e.f, filepath.Join("sub", "a.md"), ""},
		{"", e.f, "", "path is required"},
		{"/etc/passwd", e.f, "", "absolute"},
		{"~/x", e.f, "", "absolute"},
		{"shared/../media/a", e.f, "", ".."},
		{"shared/a/../../x", e.f, "", ".."},
		{"nosuch/a.md", e.f, "", "unknown root"},
		{"shared/a\x00b", e.f, "", "forbidden"},
		{"shared\\a", e.f, "", "forbidden"},
		{"shared/" + long, e.f, "", "longer than"},
		{"shared/.git/config", e.f, "", "hidden"},
		{"shared/.env", e.f, "", "hidden"},
		{"shared/d/.secret/x", e.f, "", "hidden"},
		{"shared/.env", hid.f, ".env", ""},
	}
	for _, c := range cases {
		r, rel, err := c.f.parsePath(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("parsePath(%q) err = %v, want %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil || rel != c.wantRel || r == nil {
			t.Errorf("parsePath(%q) = %v, %q, %v; want rel %q", c.in, r, rel, err, c.wantRel)
		}
	}
}

func TestSymlinkEscapeAndExtensions(t *testing.T) {
	e := newEnv(t, map[string]any{"extensions": []string{".md", ".txt"}})
	put(t, filepath.Join(e.outside, "secret.txt"), "top secret")
	put(t, filepath.Join(e.shared, "ok.md"), "fine")
	put(t, filepath.Join(e.shared, "bin.exe"), "x")
	put(t, filepath.Join(e.shared, "notes.txt"), "inside")
	if err := os.Symlink(filepath.Join(e.outside, "secret.txt"), filepath.Join(e.shared, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.outside, filepath.Join(e.shared, "outdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.shared, "notes.txt"), filepath.Join(e.shared, "inlink.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.shared, "bin.exe"), filepath.Join(e.shared, "sneaky.txt")); err != nil {
		t.Fatal(err)
	}

	e.fail(t, "files.read", `{"path":"shared/link.txt"}`, "outside its root")
	e.fail(t, "files.stat", `{"path":"shared/link.txt"}`, "outside its root")
	e.fail(t, "files.list", `{"path":"shared/outdir"}`, "outside its root")
	e.fail(t, "files.write", `{"path":"shared/outdir/new.txt","content":"x"}`, "outside its root")
	e.fail(t, "files.write", `{"path":"shared/outdir/deep/new.txt","content":"x","create_dirs":true}`, "outside its root")
	if _, err := os.Stat(filepath.Join(e.outside, "deep")); err == nil {
		t.Fatal("create_dirs escaped the root")
	}
	e.fail(t, "files.write", `{"path":"shared/link.txt","content":"x","overwrite":true}`, "outside its root")
	e.fail(t, "files.append", `{"path":"shared/link.txt","content":"x"}`, "outside its root")
	if b, _ := os.ReadFile(filepath.Join(e.outside, "secret.txt")); string(b) != "top secret" {
		t.Fatalf("outside file modified: %q", b)
	}
	if r := e.must(t, "files.read", `{"path":"shared/inlink.txt"}`); r.Content != "inside" {
		t.Errorf("inside symlink read = %q", r.Content)
	}
	e.fail(t, "files.read", `{"path":"shared/sneaky.txt"}`, "not allowed")

	e.fail(t, "files.read", `{"path":"shared/bin.exe"}`, "only .md, .txt")
	e.fail(t, "files.write", `{"path":"shared/x.exe","content":"x"}`, "only .md, .txt")

	// Listing skips symlinks and filtered extensions; folders stay.
	l := e.must(t, "files.list", `{"path":"shared","recursive":true}`)
	for _, bad := range []string{"link.txt", "outdir", "bin.exe", "secret"} {
		if strings.Contains(l.Content, bad) {
			t.Errorf("list shows %q:\n%s", bad, l.Content)
		}
	}
	if !strings.Contains(l.Content, "shared/ok.md") || !strings.Contains(l.Content, "shared/notes.txt") {
		t.Errorf("list missing files:\n%s", l.Content)
	}
	// Search never follows the symlinked directory.
	s := e.must(t, "files.search", `{"content":"top secret"}`)
	if strings.Contains(s.Content, "secret.txt") {
		t.Errorf("search followed symlink:\n%s", s.Content)
	}
}

func TestHiddenRefusedAndSkipped(t *testing.T) {
	e := newEnv(t, nil)
	put(t, filepath.Join(e.shared, ".env"), "KEY=1")
	put(t, filepath.Join(e.shared, ".git", "config"), "x")
	put(t, filepath.Join(e.shared, "visible.md"), "hello")
	e.fail(t, "files.read", `{"path":"shared/.env"}`, "hidden")
	e.fail(t, "files.write", `{"path":"shared/.env2","content":"x"}`, "hidden")
	e.fail(t, "files.delete", `{"path":"shared/.env"}`, "hidden")
	e.fail(t, "files.list", `{"path":"shared/.git"}`, "hidden")
	l := e.must(t, "files.list", `{"path":"shared","recursive":true}`)
	if strings.Contains(l.Content, ".env") || strings.Contains(l.Content, ".git") || !strings.Contains(l.Content, "visible.md") {
		t.Errorf("list:\n%s", l.Content)
	}
	// A visible symlink pointing at a hidden file is refused too.
	if err := os.Symlink(filepath.Join(e.shared, ".env"), filepath.Join(e.shared, "env.md")); err != nil {
		t.Fatal(err)
	}
	e.fail(t, "files.read", `{"path":"shared/env.md"}`, "hidden")

	h := newEnv(t, map[string]any{"include_hidden": true})
	put(t, filepath.Join(h.shared, ".env"), "KEY=1")
	if r := h.must(t, "files.read", `{"path":"shared/.env"}`); r.Content != "KEY=1" {
		t.Errorf("include_hidden read = %q", r.Content)
	}
}

func TestReadOnlyRoot(t *testing.T) {
	e := newEnv(t, nil)
	put(t, filepath.Join(e.media, "a.txt"), "ro")
	put(t, filepath.Join(e.media, "d", "b.txt"), "ro")
	if r := e.must(t, "files.read", `{"path":"media/a.txt"}`); r.Content != "ro" {
		t.Errorf("read = %q", r.Content)
	}
	for name, args := range map[string]string{
		"files.write":  `{"path":"media/n.txt","content":"x"}`,
		"files.append": `{"path":"media/a.txt","content":"x"}`,
		"files.mkdir":  `{"path":"media/newdir"}`,
		"files.delete": `{"path":"media/a.txt"}`,
		"files.move":   `{"from":"media/a.txt","to":"media/z.txt"}`,
	} {
		e.fail(t, name, args, "read-only")
	}
	if b, _ := os.ReadFile(filepath.Join(e.media, "a.txt")); string(b) != "ro" {
		t.Error("read-only file changed")
	}
}

func TestListStatRead(t *testing.T) {
	e := newEnv(t, map[string]any{"max_read": 10})
	put(t, filepath.Join(e.shared, "a.md"), "0123456789ABCDEF")
	put(t, filepath.Join(e.shared, "sub", "b.txt"), "bee")
	put(t, filepath.Join(e.shared, "sub", "deep", "c.txt"), "sea")

	l := e.must(t, "files.list", `{"path":"shared"}`)
	if !strings.Contains(l.Content, "shared/a.md") || !strings.Contains(l.Content, "shared/sub/") || strings.Contains(l.Content, "b.txt") {
		t.Errorf("non-recursive list:\n%s", l.Content)
	}
	if !l.Untrusted {
		t.Error("list must be untrusted")
	}
	l = e.must(t, "files.list", `{"path":"shared/","recursive":true}`)
	if !strings.Contains(l.Content, "shared/sub/deep/c.txt") {
		t.Errorf("recursive list:\n%s", l.Content)
	}
	l = e.must(t, "files.list", `{"path":"shared","recursive":true,"limit":2}`)
	if !strings.Contains(l.Content, "truncated") {
		t.Errorf("limit not reported:\n%s", l.Content)
	}
	if l := e.must(t, "files.list", `{}`); !strings.Contains(l.Content, "shared/") {
		t.Errorf("empty path should list roots:\n%s", l.Content)
	}
	e.fail(t, "files.list", `{"path":"shared/a.md"}`, "not a folder")
	e.fail(t, "files.list", `{"path":"shared/nope"}`, "not found")

	st := e.must(t, "files.stat", `{"path":"shared/a.md"}`)
	if !strings.Contains(st.Content, "16 B") || !strings.Contains(st.Content, "text/plain") {
		t.Errorf("stat: %s", st.Content)
	}
	if st := e.must(t, "files.stat", `{"path":"shared/sub"}`); !strings.Contains(st.Content, "folder") {
		t.Errorf("stat dir: %s", st.Content)
	}
	e.fail(t, "files.stat", `{"path":"shared/missing.md"}`, "not found")

	r := e.must(t, "files.read", `{"path":"shared/a.md"}`)
	if !strings.HasPrefix(r.Content, "0123456789\n[truncated") || !strings.Contains(r.Content, "offset=10") {
		t.Errorf("capped read: %q", r.Content)
	}
	r = e.must(t, "files.read", `{"path":"shared/a.md","offset":10}`)
	if r.Content != "ABCDEF" {
		t.Errorf("offset read: %q", r.Content)
	}
	r = e.must(t, "files.read", `{"path":"shared/a.md","offset":2,"limit":3}`)
	if !strings.HasPrefix(r.Content, "234\n[truncated") {
		t.Errorf("limit read: %q", r.Content)
	}
	e.fail(t, "files.read", `{"path":"shared/a.md","offset":99}`, "past the end")
	e.fail(t, "files.read", `{"path":"shared/sub"}`, "not a regular file")
	e.fail(t, "files.read", `{"path":"shared"}`, "folder")
}

func TestReadEncodingAndBinary(t *testing.T) {
	e := newEnv(t, nil)
	put(t, filepath.Join(e.shared, "bad.txt"), "ok \xff\xfe end")
	put(t, filepath.Join(e.shared, "bin.dat"), "\x00\x01\x02\x03binary")
	put(t, filepath.Join(e.shared, "doc.pdf"), "%PDF-1.4\n...")
	var pb bytes.Buffer
	if err := png.Encode(&pb, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	pngData := pb.Bytes()
	if err := os.WriteFile(filepath.Join(e.shared, "pic.png"), pngData, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := e.must(t, "files.read", `{"path":"shared/bad.txt"}`); r.Content != "ok \uFFFD end" {
		t.Errorf("invalid utf8: %q", r.Content)
	}
	if r := e.must(t, "files.read", `{"path":"shared/bin.dat"}`); !strings.Contains(r.Content, "binary file") || strings.Contains(r.Content, "binary\n") {
		t.Errorf("binary: %q", r.Content)
	}
	if r := e.must(t, "files.read", `{"path":"shared/doc.pdf"}`); !strings.Contains(r.Content, "PDF") || !strings.Contains(r.Content, "sandbox") {
		t.Errorf("pdf: %q", r.Content)
	}
	if r := e.must(t, "files.read", `{"path":"shared/pic.png"}`); !strings.Contains(r.Content, "3x2") {
		t.Errorf("image: %q", r.Content)
	}
}

func TestSearch(t *testing.T) {
	e := newEnv(t, map[string]any{"max_read": 100})
	put(t, filepath.Join(e.shared, "a.md"), "line one\nThe Quick brown fox\nline three\n")
	put(t, filepath.Join(e.shared, "sub", "b.txt"), "nothing here\nquick again\n")
	put(t, filepath.Join(e.media, "c.TXT"), "QUICK media\n")
	put(t, filepath.Join(e.shared, "big.txt"), strings.Repeat("quick ", 100))
	put(t, filepath.Join(e.shared, "bin.dat"), "\x00quick")
	put(t, filepath.Join(e.shared, ".hidden.txt"), "quick hidden")

	s := e.must(t, "files.search", `{"content":"quick"}`)
	for _, w := range []string{"shared/a.md:2: The Quick brown fox", "1- line one", "3- line three", "shared/sub/b.txt:2:", "media/c.TXT:1:"} {
		if !strings.Contains(s.Content, w) {
			t.Errorf("missing %q:\n%s", w, s.Content)
		}
	}
	for _, w := range []string{"big.txt", "bin.dat", "hidden"} {
		if strings.Contains(s.Content, w) {
			t.Errorf("should skip %q:\n%s", w, s.Content)
		}
	}
	if !s.Untrusted {
		t.Error("search must be untrusted")
	}
	s = e.must(t, "files.search", `{"root":"media","content":"quick"}`)
	if strings.Contains(s.Content, "shared/") || !strings.Contains(s.Content, "media/c.TXT") {
		t.Errorf("root filter:\n%s", s.Content)
	}
	s = e.must(t, "files.search", `{"name_glob":"*.TXT"}`)
	if !strings.Contains(s.Content, "shared/sub/b.txt") || !strings.Contains(s.Content, "media/c.TXT") || strings.Contains(s.Content, "a.md") {
		t.Errorf("glob:\n%s", s.Content)
	}
	s = e.must(t, "files.search", `{"name_glob":"*.md","content":"fox"}`)
	if !strings.Contains(s.Content, "shared/a.md:2:") || strings.Contains(s.Content, "b.txt") {
		t.Errorf("glob+content:\n%s", s.Content)
	}
	s = e.must(t, "files.search", `{"content":"quick","limit":1}`)
	if !strings.Contains(s.Content, "1 match") || !strings.Contains(s.Content, "truncated") {
		t.Errorf("limit:\n%s", s.Content)
	}
	if s := e.must(t, "files.search", `{"content":"zzzz"}`); s.Content != "No matches." {
		t.Errorf("no match: %q", s.Content)
	}
	e.fail(t, "files.search", `{}`, "name_glob and/or content")
	e.fail(t, "files.search", `{"name_glob":"["}`, "invalid name_glob")
	e.fail(t, "files.search", `{"root":"zzz","content":"x"}`, "unknown root")
}

func TestWriteAtomicAndOverwrite(t *testing.T) {
	e := newEnv(t, nil)
	r := e.must(t, "files.write", `{"path":"shared/r.md","content":"v1"}`)
	if !strings.Contains(r.Content, "Created") || !r.Untrusted {
		t.Errorf("write: %+v", r)
	}
	e.fail(t, "files.write", `{"path":"shared/r.md","content":"v2"}`, "overwrite=true")
	if b, _ := os.ReadFile(filepath.Join(e.shared, "r.md")); string(b) != "v1" {
		t.Fatalf("file changed without overwrite: %q", b)
	}
	if r := e.must(t, "files.write", `{"path":"shared/r.md","content":"v2","overwrite":true}`); !strings.Contains(r.Content, "Replaced") {
		t.Errorf("overwrite: %s", r.Content)
	}
	if b, _ := os.ReadFile(filepath.Join(e.shared, "r.md")); string(b) != "v2" {
		t.Fatalf("overwrite content = %q", b)
	}
	// No temp files left behind (including hidden ones).
	ents, _ := os.ReadDir(e.shared)
	if len(ents) != 1 {
		t.Errorf("leftover entries: %v", ents)
	}
	// Permissions of an existing file are preserved.
	if err := os.Chmod(filepath.Join(e.shared, "r.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.must(t, "files.write", `{"path":"shared/r.md","content":"v3","overwrite":true}`)
	if fi, _ := os.Stat(filepath.Join(e.shared, "r.md")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	// create_dirs
	e.fail(t, "files.write", `{"path":"shared/x/y/z.md","content":"c"}`, "create_dirs")
	e.must(t, "files.write", `{"path":"shared/x/y/z.md","content":"c","create_dirs":true}`)
	if b, _ := os.ReadFile(filepath.Join(e.shared, "x", "y", "z.md")); string(b) != "c" {
		t.Errorf("create_dirs content = %q", b)
	}
	// Limits and bad content.
	e.fail(t, "files.write", `{"path":"shared/big.md","content":"`+strings.Repeat("a", MaxWriteBytes+1)+`"}`, "limit")
	e.fail(t, "files.write", `{"path":"shared/nul.md","content":"a\u0000b"}`, "NUL")
	e.fail(t, "files.write", `{"path":"shared","content":"x"}`, "root")
	e.fail(t, "files.write", `{"path":"shared/x","content":"x","overwrite":true}`, "not a regular file")
}

func TestAppendMkdirDelete(t *testing.T) {
	e := newEnv(t, nil)
	e.must(t, "files.append", `{"path":"shared/log.txt","content":"one"}`)
	e.must(t, "files.append", `{"path":"shared/log.txt","content":"two\n"}`)
	e.must(t, "files.append", `{"path":"shared/log.txt","content":"three"}`)
	if b, _ := os.ReadFile(filepath.Join(e.shared, "log.txt")); string(b) != "one\ntwo\nthree" {
		t.Errorf("append = %q", b)
	}
	e.fail(t, "files.append", `{"path":"shared/nodir/log.txt","content":"x"}`, "parent folder does not exist")
	e.fail(t, "files.append", `{"path":"shared/log.txt","content":""}`, "content is required")

	e.must(t, "files.mkdir", `{"path":"shared/a/b/c"}`)
	if fi, err := os.Stat(filepath.Join(e.shared, "a", "b", "c")); err != nil || !fi.IsDir() {
		t.Fatal("mkdir did not create folders")
	}
	if r := e.must(t, "files.mkdir", `{"path":"shared/a/b/c"}`); !strings.Contains(r.Content, "already exists") {
		t.Errorf("mkdir twice: %s", r.Content)
	}
	e.fail(t, "files.mkdir", `{"path":"shared/log.txt"}`, "not a folder")
	e.fail(t, "files.mkdir", `{"path":"shared/../x"}`, "..")

	e.fail(t, "files.delete", `{"path":"shared/a"}`, "folder")
	e.fail(t, "files.delete", `{"path":"shared/nope.txt"}`, "not found")
	if r := e.must(t, "files.delete", `{"path":"shared/log.txt"}`); !strings.Contains(r.Content, "Deleted") {
		t.Errorf("delete: %s", r.Content)
	}
	if _, err := os.Stat(filepath.Join(e.shared, "log.txt")); err == nil {
		t.Error("file still exists")
	}
	// Deleting a symlink removes the link, not the target.
	put(t, filepath.Join(e.outside, "keep.txt"), "keep")
	if err := os.Symlink(filepath.Join(e.outside, "keep.txt"), filepath.Join(e.shared, "l.txt")); err != nil {
		t.Fatal(err)
	}
	e.must(t, "files.delete", `{"path":"shared/l.txt"}`)
	if b, _ := os.ReadFile(filepath.Join(e.outside, "keep.txt")); string(b) != "keep" {
		t.Error("symlink delete touched the target")
	}
}

func TestMove(t *testing.T) {
	e := newEnv(t, map[string]any{"extensions": []string{".md", ".txt"}})
	put(t, filepath.Join(e.shared, "a.md"), "a")
	put(t, filepath.Join(e.shared, "other.md"), "o")
	put(t, filepath.Join(e.shared, "d", "keep.txt"), "k")
	put(t, filepath.Join(e.media, "m.txt"), "m")

	e.must(t, "files.move", `{"from":"shared/a.md","to":"shared/sub.md"}`)
	if _, err := os.Stat(filepath.Join(e.shared, "sub.md")); err != nil {
		t.Fatal("move failed")
	}
	e.fail(t, "files.move", `{"from":"shared/sub.md","to":"media/sub.md"}`, "within one root")
	e.fail(t, "files.move", `{"from":"media/m.txt","to":"shared/m.txt"}`, "read-only")
	e.fail(t, "files.move", `{"from":"shared/sub.md","to":"shared/other.md"}`, "already exists")
	e.fail(t, "files.move", `{"from":"shared/sub.md","to":"shared/nodir/x.md"}`, "parent folder does not exist")
	e.fail(t, "files.move", `{"from":"shared/sub.md","to":"shared/x.exe"}`, "only .md, .txt")
	e.fail(t, "files.move", `{"from":"shared/sub.md","to":"shared/../x.md"}`, "..")
	e.fail(t, "files.move", `{"from":"shared/sub.md","to":"shared/.hid.md"}`, "hidden")
	e.fail(t, "files.move", `{"from":"shared","to":"shared/x"}`, "root")
	e.fail(t, "files.move", `{"from":"shared/missing.md","to":"shared/z.md"}`, "not found")
	// Folders move too.
	e.must(t, "files.move", `{"from":"shared/d","to":"shared/e"}`)
	if _, err := os.Stat(filepath.Join(e.shared, "e", "keep.txt")); err != nil {
		t.Error("folder move failed")
	}
	// Moving a symlink moves the link itself.
	put(t, filepath.Join(e.outside, "t.txt"), "t")
	if err := os.Symlink(filepath.Join(e.outside, "t.txt"), filepath.Join(e.shared, "ln.txt")); err != nil {
		t.Fatal(err)
	}
	e.must(t, "files.move", `{"from":"shared/ln.txt","to":"shared/ln2.txt"}`)
	if _, err := os.Stat(filepath.Join(e.outside, "t.txt")); err != nil {
		t.Error("target disappeared")
	}
	e.fail(t, "files.read", `{"path":"shared/ln2.txt"}`, "outside its root")
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"2MiB": 2 << 20, "512KiB": 512 << 10, "1MB": 1 << 20, "100": 100, "1 GiB": 1 << 30, "3k": 3072} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "2XB", "-1"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q): expected error", in)
		}
	}
}
