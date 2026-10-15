package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" // the sandbox must drive the docker CLI; see integrations_test.go
	"strings"
	"sync"
	"time"
)

// Label marks every container the sandbox creates, so leftovers from a
// crashed or killed daemon can be found and removed.
const Label = "bosun.sandbox=1"

// wrapper runs inside the container as `sh -c wrapper bosun <cmd...>`. It
// unpacks the input archive from stdin into /work, runs the command with
// the request's stdin and with stdout/stderr captured to files, then writes
// a result archive (exit status, stdout, stderr, out/) to its own stdout.
// The command arrives as positional parameters, so nothing is ever
// interpolated into shell source.
const wrapper = `tar -xf - -C /work >/dev/null 2>&1 || { echo "bosun sandbox: could not unpack inputs (does the image have tar?)" >&2; exit 121; }
mkdir -p /work/out /work/tmp
cd /work
"$@" </work/.bosun/stdin >/work/.bosun/stdout 2>/work/.bosun/stderr
echo "$?" >/work/.bosun/exit
cd /work && exec tar -cf - .bosun/exit .bosun/stdout .bosun/stderr out`

// runArgs builds the `docker run` argument list for one run. Nothing on the
// host is mounted: the only writable places are tmpfs mounts, inputs arrive
// on stdin and outputs leave on stdout.
func runArgs(s Settings, name, image string, cmd []string) []string {
	args := []string{
		"run", "--rm", "-i",
		"--name", name,
		"--label", Label,
		"--pull", "never",
		"--log-driver", "none",
		"--network", "none",
		"--read-only",
		"--tmpfs", "/work:rw,exec,nosuid,nodev,size=" + s.WorkSize + ",mode=1777",
		"--workdir", "/work",
		"--memory", s.Memory,
		"--memory-swap", s.Memory,
		"--cpus", "1",
		"--pids-limit", "256",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65534:65534",
		"--env", "HOME=/work",
		"--env", "TMPDIR=/work/tmp",
		"--env", "PYTHONDONTWRITEBYTECODE=1",
		"--entrypoint", "sh",
		image,
		"-c", wrapper, "bosun",
	}
	return append(args, cmd...)
}

// dockerEnv is the environment the docker CLI runs with: enough to find the
// daemon and the CLI's config, and nothing else from bosund's environment.
func dockerEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "PATH", k == "HOME", k == "USER", k == "TMPDIR", k == "XDG_RUNTIME_DIR",
			strings.HasPrefix(k, "DOCKER_"):
			env = append(env, kv)
		}
	}
	return env
}

// docker runs the docker CLI.
type docker struct {
	bin string
	env []string
}

// output runs a short docker command and returns its trimmed stdout. Errors
// include (a bounded amount of) stderr.
func (d docker) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Env = d.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: 1 << 20}
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4 << 10}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// serverVersion reports whether the docker daemon answers.
func (d docker) serverVersion(ctx context.Context) (string, error) {
	return d.output(ctx, "version", "--format", "{{.Server.Version}}")
}

func (d docker) imagePresent(ctx context.Context, image string) (bool, error) {
	_, err := d.output(ctx, "image", "inspect", "--format", "{{.Id}}", image)
	if err == nil {
		return true, nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "no such image") {
		return false, nil
	}
	return false, err
}

// removeLabelled force-removes every container carrying Label whose name
// starts with prefix.
func (d docker) removeLabelled(ctx context.Context, prefix string) (int, error) {
	out, err := d.output(ctx, "ps", "-aq", "--filter", "label="+Label, "--filter", "name=^/?"+prefix)
	if err != nil {
		return 0, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := d.output(ctx, append([]string{"rm", "-f"}, ids...)...); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// errTimeout is returned when a run exceeded its time limit.
var errTimeout = errors.New("timed out")

// runContainer starts `docker run` with input on stdin, parses the result
// archive from stdout, and enforces timeout by force-removing the container.
func (d docker) runContainer(ctx context.Context, args []string, name string, input []byte, timeout time.Duration) (*Output, error) {
	cmd := exec.Command(d.bin, args...)
	cmd.Env = d.env
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start docker: %w", err)
	}
	type result struct {
		out     *Output
		perr    error
		waitErr error
	}
	done := make(chan result, 1)
	go func() {
		out, perr := parseOutputTar(stdout)
		_, _ = io.Copy(io.Discard, stdout) // drain so the CLI can exit
		done <- result{out, perr, cmd.Wait()}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var res result
	var stopErr error
	select {
	case res = <-done:
	case <-timer.C:
		stopErr = fmt.Errorf("%w after %s", errTimeout, timeout)
	case <-ctx.Done():
		stopErr = ctx.Err()
	}
	if stopErr != nil {
		// Killing the CLI does not stop the container; remove it.
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		_, rmErr := d.output(rmCtx, "rm", "-f", name)
		cancel()
		_ = cmd.Process.Kill()
		<-done
		if rmErr != nil && !strings.Contains(strings.ToLower(rmErr.Error()), "no such container") {
			return nil, fmt.Errorf("%w (and removing container %s failed: %v)", stopErr, name, rmErr)
		}
		return nil, stopErr
	}
	if res.perr != nil {
		msg := strings.TrimSpace(stderr.String())
		code := -1
		var ee *exec.ExitError
		if errors.As(res.waitErr, &ee) {
			code = ee.ExitCode()
		}
		switch {
		case code == 137:
			return nil, errors.New("the sandbox was killed (exit 137; it probably ran out of memory)")
		case msg != "":
			return nil, fmt.Errorf("docker run failed (exit %d): %s", code, msg)
		case res.waitErr != nil:
			return nil, fmt.Errorf("docker run failed: %v", res.waitErr)
		default:
			return nil, res.perr
		}
	}
	return res.out, nil
}

// limitedWriter keeps the first n bytes and discards the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// puller pulls each image at most once at a time. A pull is detached from
// the caller's context, so a tool call that times out while a large image
// downloads does not abort it; the next call finds the image present.
type puller struct {
	mu       sync.Mutex
	inflight map[string]*pull
}

type pull struct {
	done chan struct{}
	err  error
}

func (p *puller) ensure(ctx context.Context, d docker, image string) error {
	p.mu.Lock()
	if p.inflight == nil {
		p.inflight = map[string]*pull{}
	}
	pl, ok := p.inflight[image]
	if !ok {
		pl = &pull{done: make(chan struct{})}
		p.inflight[image] = pl
		go func() {
			pctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			_, pl.err = d.output(pctx, "pull", "--quiet", image)
			close(pl.done)
			p.mu.Lock()
			delete(p.inflight, image)
			p.mu.Unlock()
		}()
	}
	p.mu.Unlock()
	select {
	case <-pl.done:
		return pl.err
	case <-ctx.Done():
		return fmt.Errorf("image %s is still being pulled; try again shortly", image)
	}
}
