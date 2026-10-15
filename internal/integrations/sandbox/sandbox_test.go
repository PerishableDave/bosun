package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/integration"
)

// TestYAMLConfig checks that the README's bosun.yaml example survives the
// config package's YAML -> JSON path and is accepted.
func TestYAMLConfig(t *testing.T) {
	var ic config.IntegrationConfig
	src := `
enabled: true
backend: docker
images:
  python: local/bosun-python-data:1
timeout: 90s
memory: 1g
work_size: 128m
max_concurrent: 3
pull: never
`
	if err := yaml.Unmarshal([]byte(src), &ic); err != nil {
		t.Fatal(err)
	}
	raw, err := ic.SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseSettings(raw)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if time.Duration(s.Timeout) != 90*time.Second || s.Memory != "1g" || s.WorkSize != "128m" ||
		s.MaxConcurrent != 3 || s.Pull != "never" || s.Images["python"] != "local/bosun-python-data:1" {
		t.Fatalf("settings = %+v", s)
	}
}

func TestParseSettingsDefaults(t *testing.T) {
	s, err := parseSettings(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Backend != "docker" || s.Docker != "docker" || s.Memory != "512m" || s.WorkSize != "64m" ||
		s.Pull != "auto" || s.MaxConcurrent != 2 || time.Duration(s.Timeout) != 60*time.Second {
		t.Fatalf("defaults = %+v", s)
	}
	for lang, img := range DefaultImages {
		if s.Images[lang] != img {
			t.Errorf("image %s = %q", lang, s.Images[lang])
		}
	}
}

func TestParseSettings(t *testing.T) {
	s, err := parseSettings(json.RawMessage(`{"timeout":"90s","memory":"1g","images":{"python":"ghcr.io/me/py-pandas:1.0"},"pull":"never","max_concurrent":4}`))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(s.Timeout) != 90*time.Second || s.Memory != "1g" || s.Pull != "never" || s.MaxConcurrent != 4 {
		t.Fatalf("got %+v", s)
	}
	if s.Images["python"] != "ghcr.io/me/py-pandas:1.0" || s.Images["node"] != "node:22-alpine" {
		t.Fatalf("images = %v", s.Images)
	}
	if s, err := parseSettings(json.RawMessage(`{"timeout":30}`)); err != nil || time.Duration(s.Timeout) != 30*time.Second {
		t.Fatalf("numeric timeout: %v %v", s.Timeout, err)
	}
	for _, bad := range []string{
		`{"backend":"podman"}`,
		`{"timeout":"301s"}`,
		`{"timeout":"0.5s"}`,
		`{"timeout":"soon"}`,
		`{"memory":"lots"}`,
		`{"memory":"-1"}`,
		`{"work_size":"64 m"}`,
		`{"pull":"always"}`,
		`{"max_concurrent":-1}`,
		`{"max_concurrent":17}`,
		`{"images":{"ruby":"ruby:3"}}`,
		`{"images":{"python":"--privileged"}}`,
		`{"images":{"python":"python:3 --network host"}}`,
		`{"unknown":1}`,
	} {
		if _, err := parseSettings(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestBackendNone(t *testing.T) {
	s := New()
	if len(s.Tools()) != 1 || len(s.Manifest().Capabilities) != 1 {
		t.Fatal("before Init the manifest declares sandbox.run")
	}
	if err := integration.Validate(s); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(context.Background(), integration.Config{Name: "sandbox", Settings: json.RawMessage(`{"backend":"none"}`)}, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.Tools()) != 0 || len(s.Manifest().Capabilities) != 0 {
		t.Fatal("backend none must declare no tools or capabilities")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManifestAndTool(t *testing.T) {
	s := New()
	m := s.Manifest()
	if m.Name != "sandbox" || m.Scope != integration.ScopeShared || !m.HasCapability("sandbox.run") {
		t.Fatalf("manifest = %+v", m)
	}
	tools := s.Tools()
	if len(tools) != 1 || tools[0].Name != "sandbox.run" || tools[0].Risk != integration.RiskWrite || tools[0].Capability != "sandbox.run" {
		t.Fatalf("tools = %+v", tools)
	}
	if !json.Valid(tools[0].InputSchema) || !json.Valid(m.ConfigSchema) {
		t.Fatal("invalid schema")
	}
}

func TestRunArgsExactFlags(t *testing.T) {
	s, _ := parseSettings(json.RawMessage(`{"memory":"256m","work_size":"32m"}`))
	got := runArgs(s, "bosun-sbx-abc", "python:3.12-alpine", []string{"python3", "main.py", "--x", "$(id)"})
	want := []string{
		"run", "--rm", "-i",
		"--name", "bosun-sbx-abc",
		"--label", "bosun.sandbox=1",
		"--pull", "never",
		"--log-driver", "none",
		"--network", "none",
		"--read-only",
		"--tmpfs", "/work:rw,exec,nosuid,nodev,size=32m,mode=1777",
		"--workdir", "/work",
		"--memory", "256m",
		"--memory-swap", "256m",
		"--cpus", "1",
		"--pids-limit", "256",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65534:65534",
		"--env", "HOME=/work",
		"--env", "TMPDIR=/work/tmp",
		"--env", "PYTHONDONTWRITEBYTECODE=1",
		"--entrypoint", "sh",
		"python:3.12-alpine",
		"-c", wrapper, "bosun",
		"python3", "main.py", "--x", "$(id)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("runArgs:\n got %q\nwant %q", got, want)
	}
	for _, a := range got {
		if a == "-v" || a == "--volume" || a == "--mount" || a == "--privileged" || strings.HasPrefix(a, "--network=host") {
			t.Fatalf("forbidden flag %q", a)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"data.csv":               "data.csv",
		"../../etc/passwd":       "passwd",
		"/abs/path/x.txt":        "x.txt",
		`C:\dir\win.txt`:         "win.txt",
		".hidden":                "hidden",
		"..":                     "",
		".":                      "",
		"":                       "",
		"a b$c.txt":              "a_b_c.txt",
		"dir/":                   "dir",
		"日本.txt":                 "__.txt",
		"???":                    "",
		strings.Repeat("a", 300): strings.Repeat("a", 100),
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := request{Language: "python", Code: "print(1)", Files: map[string]string{"b.txt": "b", "a/../a.txt": "a"}}
	files, err := ok.validate()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "a.txt" || files[1].Name != "b.txt" {
		t.Fatalf("files = %+v", files)
	}
	big := strings.Repeat("x", MaxFileBytes)
	manyBig := map[string]string{}
	for i := range 9 {
		manyBig[string(rune('a'+i))+".txt"] = big
	}
	tooMany := map[string]string{}
	for i := range MaxInputFiles + 1 {
		tooMany[strings.Repeat("f", i+1)] = "x"
	}
	for name, r := range map[string]request{
		"language":   {Language: "ruby", Code: "x"},
		"no code":    {Language: "python", Code: "  "},
		"huge code":  {Language: "python", Code: strings.Repeat("x", MaxCodeBytes+1)},
		"huge stdin": {Language: "python", Code: "x", Stdin: strings.Repeat("x", MaxStdinBytes+1)},
		"huge file":  {Language: "python", Code: "x", Files: map[string]string{"a": big + "x"}},
		"total":      {Language: "python", Code: "x", Files: manyBig},
		"count":      {Language: "python", Code: "x", Files: tooMany},
		"bad name":   {Language: "python", Code: "x", Files: map[string]string{"..": "x"}},
		"collision":  {Language: "python", Code: "x", Files: map[string]string{"a/x.txt": "1", "b/x.txt": "2"}},
		"many args":  {Language: "python", Code: "x", Args: make([]string, MaxArgs+1)},
		"long arg":   {Language: "python", Code: "x", Args: []string{strings.Repeat("x", MaxArgBytes+1)}},
		"nul arg":    {Language: "python", Code: "x", Args: []string{"a\x00b"}},
	} {
		if _, err := r.validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if err := decodeStrict(json.RawMessage(`{"language":"python","code":"x","network":true}`), &request{}); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestHandlerValidatesBeforeDocker(t *testing.T) {
	s := New()
	_, err := s.handleRun(context.Background(), json.RawMessage(`{"language":"cobol","code":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "language") {
		t.Fatalf("err = %v", err)
	}
	_, err = s.handleRun(context.Background(), json.RawMessage(`{"language":"python","code":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("uninitialised run: err = %v", err)
	}
}

func TestInputTar(t *testing.T) {
	r := request{Language: "node", Code: "console.log(1)", Stdin: "in", Files: map[string]string{"x.json": "{}"}}
	files, err := r.validate()
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildInputTar(&r, files, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		got[h.Name] = string(c)
	}
	want := map[string]string{".bosun/": "", ".bosun/stdin": "in", "main.js": "console.log(1)", "in/": "", "in/x.json": "{}"}
	if len(got) != len(want) {
		t.Fatalf("entries = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

type tarEntry struct {
	name, body string
	typ        byte
}

func mkTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if typ == tar.TypeSymlink {
			h.Linkname = e.body
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

func TestParseOutputTar(t *testing.T) {
	var entries []tarEntry
	entries = append(entries,
		tarEntry{name: ".bosun/exit", body: "3\n"},
		tarEntry{name: ".bosun/stdout", body: strings.Repeat("o", MaxStreamBytes+10)},
		tarEntry{name: ".bosun/stderr", body: "warn\n"},
		tarEntry{name: "out/", typ: tar.TypeDir},
		tarEntry{name: "out/a.txt", body: "hello"},
		tarEntry{name: "out/bin.dat", body: "\x00\x01\x02"},
		tarEntry{name: "out/link", body: "/etc/passwd", typ: tar.TypeSymlink},
		tarEntry{name: "other.txt", body: "not collected"},
	)
	for i := range MaxOutputFiles {
		entries = append(entries, tarEntry{name: "out/f" + strings.Repeat("x", i), body: "1"})
	}
	out, err := parseOutputTar(bytes.NewReader(mkTar(t, entries...)))
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 3 || out.Stderr != "warn\n" || !out.StdoutTruncated || len(out.Stdout) != MaxStreamBytes {
		t.Fatalf("out = exit %d stderr %q trunc %v len %d", out.ExitCode, out.Stderr, out.StdoutTruncated, len(out.Stdout))
	}
	if f := out.Files["a.txt"]; f.Encoding != "utf8" || f.Content != "hello" || f.Size != 5 {
		t.Errorf("a.txt = %+v", f)
	}
	if f := out.Files["bin.dat"]; f.Encoding != "base64" || f.Content != "AAEC" {
		t.Errorf("bin.dat = %+v", f)
	}
	if _, ok := out.Files["link"]; ok || !slices.Contains(out.Omitted, "link") {
		t.Error("symlink must be omitted")
	}
	if len(out.Files) != MaxOutputFiles {
		t.Errorf("collected %d files, want cap %d", len(out.Files), MaxOutputFiles)
	}
	rep := out.report()
	for _, want := range []string{"exit code: 3", "--- stdout (truncated", "out/a.txt (5 bytes):\nhello", "out/bin.dat (3 bytes, binary)", "not collected"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report missing %q:\n%s", want, rep[len(rep)-min(len(rep), 2000):])
		}
	}
	if _, err := json.Marshal(out); err != nil {
		t.Fatal(err)
	}

	// Size cap across files.
	big := strings.Repeat("b", MaxOutputBytes/2+1)
	out, err = parseOutputTar(bytes.NewReader(mkTar(t,
		tarEntry{name: ".bosun/exit", body: "0"},
		tarEntry{name: "out/1", body: big}, tarEntry{name: "out/2", body: big})))
	if err != nil || len(out.Files) != 1 || !slices.Contains(out.Omitted, "2") {
		t.Fatalf("size cap: files %d omitted %v err %v", len(out.Files), out.Omitted, err)
	}

	// No exit status: the wrapper did not finish.
	if _, err := parseOutputTar(bytes.NewReader(mkTar(t, tarEntry{name: ".bosun/stdout", body: "x"}))); !errors.Is(err, errNoExit) {
		t.Fatalf("err = %v", err)
	}
	if _, err := parseOutputTar(strings.NewReader("")); !errors.Is(err, errNoExit) {
		t.Fatalf("empty: err = %v", err)
	}
}

func TestDockerEnvIsMinimal(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	t.Setenv("BOSUN_MASTER_KEY", "secret")
	t.Setenv("DOCKER_HOST", "unix:///x.sock")
	env := dockerEnv()
	if slices.ContainsFunc(env, func(kv string) bool { return strings.Contains(kv, "secret") }) {
		t.Fatalf("env leaks secrets: %v", env)
	}
	if !slices.Contains(env, "DOCKER_HOST=unix:///x.sock") {
		t.Fatalf("env lacks DOCKER_HOST: %v", env)
	}
}

func TestBusy(t *testing.T) {
	s := New()
	s.enabled = true
	s.sem = make(chan struct{}, 1)
	s.sem <- struct{}{}
	_, err := s.run(context.Background(), &request{Language: "python", Code: "x"}, nil)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
}
