package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/integration"
)

// Live tests: they need a working docker daemon and are skipped otherwise.
// Containers they create are named bosun-sbx-test-*, so a real daemon's
// sandboxes on the same machine are never touched.

const testPrefix = "bosun-sbx-test-"

func requireDocker(t *testing.T) docker {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping docker tests")
	}
	bin, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not installed")
	}
	d := docker{bin: bin, env: dockerEnv()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.serverVersion(ctx); err != nil {
		t.Skipf("docker not reachable: %v", err)
	}
	return d
}

func newLive(t *testing.T, settings string) *Integration {
	t.Helper()
	d := requireDocker(t)
	s := New(WithNamePrefix(testPrefix))
	if err := s.Init(context.Background(), integration.Config{Name: "sandbox", Settings: json.RawMessage(settings)}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if left := listTest(t, ctx, d); left != "" {
			t.Errorf("containers left after Close: %s", left)
		}
	})
	return s
}

func listTest(t *testing.T, ctx context.Context, d docker) string {
	t.Helper()
	out, err := d.output(ctx, "ps", "-a", "--format", "{{.Names}}", "--filter", "name=^/?"+testPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func call(t *testing.T, s *Integration, ctx context.Context, args map[string]any) (*Output, integration.Result, error) {
	t.Helper()
	in, _ := json.Marshal(args)
	res, err := s.handleRun(ctx, in)
	if err != nil {
		return nil, res, err
	}
	if !res.Untrusted {
		t.Error("sandbox results must be Untrusted")
	}
	out, _ := res.Data.(*Output)
	return out, res, nil
}

func mustRun(t *testing.T, s *Integration, args map[string]any) (*Output, integration.Result) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) // first run may pull
	defer cancel()
	out, res, err := call(t, s, ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	return out, res
}

func TestLiveHelloWorld(t *testing.T) {
	s := newLive(t, `{}`)
	for _, tc := range []struct{ lang, code string }{
		{"python", `import sys; print("hello from python", sys.argv[1:], sys.stdin.read())`},
		{"node", `let d='';process.stdin.on('data',c=>d+=c).on('end',()=>console.log("hello from node", process.argv.slice(2), d))`},
		{"shell", `echo "hello from shell" "$@" "$(cat)"`},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			out, res := mustRun(t, s, map[string]any{"language": tc.lang, "code": tc.code, "args": []string{"a b", "$(id)"}, "stdin": "IN"})
			if out.ExitCode != 0 || !strings.Contains(out.Stdout, "hello from "+tc.lang) ||
				!strings.Contains(out.Stdout, "$(id)") || !strings.Contains(out.Stdout, "IN") {
				t.Fatalf("exit %d stdout %q stderr %q", out.ExitCode, out.Stdout, out.Stderr)
			}
			if !strings.Contains(res.Content, "exit code: 0") {
				t.Errorf("content = %q", res.Content)
			}
		})
	}
	st := s.Status()
	if st.State != integration.StatusOK {
		t.Errorf("status = %+v", st)
	}
}

func TestLiveFilesRoundTrip(t *testing.T) {
	s := newLive(t, `{}`)
	out, res := mustRun(t, s, map[string]any{
		"language": "python",
		"code": `import os
data = open("in/data.csv").read()
os.makedirs("out/sub", exist_ok=True)
open("out/upper.txt", "w").write(data.upper())
open("out/sub/n.txt", "w").write(str(len(data)))
open("out/blob.bin", "wb").write(bytes(range(256)))
os.symlink("/etc/passwd", "out/link")
print(sorted(os.listdir("in")))`,
		"files": map[string]string{"../../data.csv": "a,b\n1,2\n", "notes.txt": "n"},
	})
	if out.ExitCode != 0 {
		t.Fatalf("exit %d: %s", out.ExitCode, out.Stderr)
	}
	if !strings.Contains(out.Stdout, "['data.csv', 'notes.txt']") {
		t.Errorf("stdout = %q", out.Stdout)
	}
	if f := out.Files["upper.txt"]; f.Content != "A,B\n1,2\n" || f.Encoding != "utf8" {
		t.Errorf("upper.txt = %+v", f)
	}
	if f := out.Files["sub/n.txt"]; f.Content != "8" {
		t.Errorf("sub/n.txt = %+v", f)
	}
	if f := out.Files["blob.bin"]; f.Encoding != "base64" || f.Size != 256 {
		t.Errorf("blob.bin = %+v", f)
	}
	if _, ok := out.Files["link"]; ok {
		t.Error("symlink collected")
	}
	if !strings.Contains(res.Content, "out/upper.txt (8 bytes):\nA,B") {
		t.Errorf("content = %s", res.Content)
	}
}

func TestLiveNoNetwork(t *testing.T) {
	s := newLive(t, `{}`)
	start := time.Now()
	out, _ := mustRun(t, s, map[string]any{"language": "python", "code": `import socket
for target in [("1.1.1.1", 53), ("example.com", 80)]:
    try:
        socket.create_connection(target, timeout=5)
        print("CONNECTED", target)
    except Exception as e:
        print("blocked:", type(e).__name__)
import urllib.request
try:
    urllib.request.urlopen("http://example.com", timeout=5)
    print("CONNECTED urllib")
except Exception as e:
    print("blocked:", type(e).__name__)
routes = open("/proc/net/route").read().splitlines()[1:]
print("routes:", len(routes))`})
	if strings.Contains(out.Stdout, "CONNECTED") || strings.Count(out.Stdout, "blocked:") != 3 {
		t.Fatalf("network reachable? stdout %q stderr %q", out.Stdout, out.Stderr)
	}
	if !strings.Contains(out.Stdout, "routes: 0") { // only loopback, which has no route entry
		t.Errorf("routes: %q", out.Stdout)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("network failure was not fast: %s", d)
	}
}

func TestLiveIsolation(t *testing.T) {
	s := newLive(t, `{}`)
	out, _ := mustRun(t, s, map[string]any{"language": "shell", "code": `
echo "uid=$(id -u) gid=$(id -g)"
ls /
touch /etc/bosun-test 2>/dev/null && echo "ROOT WRITABLE" || echo "root read-only"
touch /tmp/x 2>/dev/null && echo "TMP WRITABLE" || echo "tmp read-only"
touch /work/x && echo "work writable"
test -e /var/run/docker.sock && echo "DOCKER SOCKET" || echo "no docker socket"
cat /proc/self/status | grep CapEff
cat /proc/mounts
`})
	if out.ExitCode != 0 {
		t.Fatalf("exit %d: %s", out.ExitCode, out.Stderr)
	}
	o := out.Stdout
	for _, want := range []string{"uid=65534 gid=65534", "root read-only", "tmp read-only", "work writable", "no docker socket", "CapEff:\t0000000000000000"} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %q in:\n%s", want, o)
		}
	}
	// Only the image's own top-level directories, plus Docker's standard
	// mount points.
	for _, bad := range []string{"Users", "home/runner", "docker.sock"} {
		if strings.Contains(o, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, o)
		}
	}
	// Mounts: the only non-Docker-internal mount is our tmpfs. Docker
	// itself provides /etc/hosts, /etc/hostname and /etc/resolv.conf.
	for _, line := range strings.Split(o, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[1], "/") {
			continue
		}
		switch {
		case f[1] == "/" && strings.Contains(f[3], "ro"):
		case f[1] == "/work" && f[2] == "tmpfs":
		case strings.HasPrefix(f[1], "/proc"), strings.HasPrefix(f[1], "/sys"), strings.HasPrefix(f[1], "/dev"):
		case f[1] == "/etc/hosts", f[1] == "/etc/hostname", f[1] == "/etc/resolv.conf":
		case f[1] == "/":
			t.Errorf("root not read-only: %s", line)
		default:
			t.Errorf("unexpected mount: %s", line)
		}
	}
}

func TestLiveTimeoutKills(t *testing.T) {
	d := requireDocker(t)
	s := newLive(t, `{"timeout":"3s"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	mustRun(t, s, map[string]any{"language": "shell", "code": "true"}) // warm up / pull
	start := time.Now()
	_, _, err := call(t, s, ctx, map[string]any{"language": "shell", "code": "while :; do :; done"})
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("err = %v", err)
	}
	if took > 8*time.Second {
		t.Errorf("timeout took %s (limit 3s + 5s)", took)
	}
	if left := listTest(t, ctx, d); left != "" {
		t.Errorf("container still present after timeout: %s", left)
	}
}

func TestLiveCallerDeadlineShortensRun(t *testing.T) {
	s := newLive(t, `{}`)
	mustRun(t, s, map[string]any{"language": "shell", "code": "true"})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_, _, err := call(t, s, ctx, map[string]any{"language": "shell", "code": "sleep 100"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if ctx.Err() != nil {
		t.Error("the run should end before the caller's deadline")
	}
}

func TestLiveMemoryLimit(t *testing.T) {
	s := newLive(t, `{}`) // default 512m
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, _, err := call(t, s, ctx, map[string]any{"language": "python", "code": `b = bytearray(2 * 1024**3)
b[::4096] = b"x" * (len(b) // 4096)
print("ALLOCATED")`})
	if err != nil {
		// The whole container was OOM-killed: also a failure to allocate.
		if !strings.Contains(err.Error(), "memory") {
			t.Fatalf("err = %v", err)
		}
		return
	}
	if out.ExitCode == 0 || strings.Contains(out.Stdout, "ALLOCATED") {
		t.Fatalf("2 GiB allocation succeeded: exit %d stdout %q", out.ExitCode, out.Stdout)
	}
	t.Logf("allocation failed as expected: exit %d, stderr %q", out.ExitCode, lastLine(out.Stderr))
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func TestLivePidsLimit(t *testing.T) {
	s := newLive(t, `{}`)
	out, _ := mustRun(t, s, map[string]any{"language": "python", "code": `import os, signal, time
pids, err = [], None
try:
    for i in range(400):
        pid = os.fork()
        if pid == 0:
            time.sleep(30)
            os._exit(0)
        pids.append(pid)
except OSError as e:
    err = e
for p in pids:
    os.kill(p, signal.SIGKILL)
for p in pids:
    os.waitpid(p, 0)
print("forked", len(pids), "error", type(err).__name__)`})
	t.Logf("pids: %q", out.Stdout)
	if !strings.Contains(out.Stdout, "error BlockingIOError") || strings.Contains(out.Stdout, "forked 400") {
		t.Fatalf("pids limit not enforced: stdout %q stderr %q", out.Stdout, out.Stderr)
	}
}

func TestLiveConcurrencyLimit(t *testing.T) {
	s := newLive(t, `{"max_concurrent":1}`)
	mustRun(t, s, map[string]any{"language": "shell", "code": "true"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	started := make(chan struct{})
	var firstErr error
	go func() {
		defer wg.Done()
		close(started)
		_, _, firstErr = call(t, s, ctx, map[string]any{"language": "shell", "code": "sleep 4"})
	}()
	<-started
	time.Sleep(1 * time.Second)
	_, _, err := call(t, s, ctx, map[string]any{"language": "shell", "code": "echo second"})
	if !errors.Is(err, ErrBusy) {
		t.Errorf("second concurrent run: err = %v, want busy", err)
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("first run: %v", firstErr)
	}
	out, _ := mustRun(t, s, map[string]any{"language": "shell", "code": "echo after"})
	if !strings.Contains(out.Stdout, "after") {
		t.Errorf("run after the slot freed: %q", out.Stdout)
	}
}

func TestLiveLeftoverCleanup(t *testing.T) {
	d := requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Make sure the image exists, then simulate a daemon that died mid-run.
	if ok, _ := d.imagePresent(ctx, DefaultImages[LangShell]); !ok {
		if _, err := d.output(ctx, "pull", "--quiet", DefaultImages[LangShell]); err != nil {
			t.Fatal(err)
		}
	}
	name := testPrefix + "leftover-" + randomID()
	if _, err := d.output(ctx, "run", "-d", "--name", name, "--label", Label, "--network", "none", DefaultImages[LangShell], "sleep", "300"); err != nil {
		t.Fatal(err)
	}
	other := "bosun-sbx-unrelated-" + randomID() // not ours: no label
	if _, err := d.output(ctx, "create", "--name", other, DefaultImages[LangShell], "true"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = d.output(context.Background(), "rm", "-f", other) }()

	s := New(WithNamePrefix(testPrefix))
	if err := s.Init(ctx, integration.Config{Name: "sandbox", Settings: json.RawMessage(`{}`)}, nil); err != nil {
		t.Fatal(err)
	}
	if left := listTest(t, ctx, d); left != "" {
		t.Errorf("Init left %q", left)
	}
	if _, err := d.output(ctx, "inspect", other); err != nil {
		t.Errorf("unlabelled container was removed: %v", err)
	}
	// Close also sweeps.
	if _, err := d.output(ctx, "create", "--name", name, "--label", Label, DefaultImages[LangShell], "true"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if left := listTest(t, ctx, d); left != "" {
		t.Errorf("Close left %q", left)
	}
}

func TestLivePullNever(t *testing.T) {
	s := newLive(t, `{"pull":"never","images":{"shell":"bosun-sbx-test/does-not-exist:0"}}`)
	if st := s.Status(); st.State != integration.StatusDegraded {
		t.Errorf("status = %+v", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _, err := call(t, s, ctx, map[string]any{"language": "shell", "code": "true"})
	if err == nil || !strings.Contains(err.Error(), "docker pull bosun-sbx-test/does-not-exist:0") {
		t.Fatalf("err = %v", err)
	}
}
