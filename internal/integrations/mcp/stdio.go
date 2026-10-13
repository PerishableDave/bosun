package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec" // the one integration allowed to spawn processes; see integrations_test.go
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// maxLineBytes bounds one newline-delimited JSON-RPC message from a server.
const maxLineBytes = 16 << 20

// stdioTransport speaks newline-delimited JSON-RPC to a child process. A
// reader goroutine demultiplexes responses to waiting callers by id.
type stdioTransport struct {
	name      string
	log       *slog.Logger
	cmd       *exec.Cmd
	killGrace time.Duration

	stdin   *os.File // our write end of the child's stdin
	stdout  *os.File // our read end of the child's stdout
	stderr  *os.File
	writeMu sync.Mutex

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcMessage

	deadOnce sync.Once
	dead     chan struct{} // closed when the connection is unusable
	deadErr  error

	exited  chan struct{} // closed when Wait returns
	exitErr error         // valid after exited is closed

	closeOnce sync.Once
}

// startStdio spawns command with exactly env (no inheritance) and starts the
// reader goroutines. The process is not tied to any context: it lives until
// close or until it exits by itself.
func startStdio(name string, command []string, env []string, dir string, killGrace time.Duration, log *slog.Logger) (*stdioTransport, error) {
	if len(command) == 0 {
		return nil, errors.New("empty command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = env
	if cmd.Env == nil {
		cmd.Env = []string{} // never inherit the daemon's environment
	}
	cmd.Dir = dir

	// Explicit os.Pipe pairs (rather than cmd.StdoutPipe) so that cmd.Wait
	// never races our reads and we control when each end closes.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	err = cmd.Start()
	// The child has its own copies now; ours must close so EOF propagates.
	inR.Close()
	outW.Close()
	errW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return nil, fmt.Errorf("start %s: %w", command[0], err)
	}
	t := &stdioTransport{
		name: name, log: log, cmd: cmd, killGrace: killGrace,
		stdin: inW, stdout: outR, stderr: errR,
		pending: map[int64]chan rpcMessage{},
		dead:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		t.readLoop()
	}()
	go t.stderrLoop()
	go func() {
		t.exitErr = cmd.Wait()
		close(t.exited)
		// Give the reader a moment to drain anything the child wrote just
		// before exiting, then make sure it stops even if a grandchild
		// inherited stdout and keeps it open.
		select {
		case <-readerDone:
		case <-time.After(time.Second):
			t.stdout.Close()
		}
		cause := t.exitErr
		if cause == nil {
			cause = errors.New("exited")
		}
		t.markDead(notRunning(t.name, cause))
	}()
	return t, nil
}

func (t *stdioTransport) markDead(err error) {
	t.deadOnce.Do(func() {
		t.mu.Lock()
		t.deadErr = err
		t.mu.Unlock()
		close(t.dead)
	})
}

func (t *stdioTransport) err() error {
	select {
	case <-t.dead:
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.deadErr
	default:
		return nil
	}
}

func (t *stdioTransport) setProtocolVersion(string) {}

func (t *stdioTransport) readLoop() {
	sc := bufio.NewScanner(t.stdout)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			t.log.Debug("mcp: ignoring non-JSON-RPC line on stdout", "server", t.name, "err", err)
			continue
		}
		switch {
		case m.isResponse():
			id, ok := m.numericID()
			if !ok {
				t.log.Debug("mcp: response with unknown id", "server", t.name, "id", string(m.ID))
				continue
			}
			t.mu.Lock()
			ch := t.pending[id]
			delete(t.pending, id)
			t.mu.Unlock()
			if ch != nil {
				ch <- m // buffered 1
			}
		case m.isRequest():
			go func() {
				if err := t.write(context.Background(), replyTo(m)); err != nil {
					t.log.Debug("mcp: reply to server request failed", "server", t.name, "method", m.Method, "err", err)
				}
			}()
		default:
			t.log.Debug("mcp: notification", "server", t.name, "method", m.Method)
		}
	}
	err := sc.Err()
	if err == nil {
		err = errors.New("stdout closed")
	}
	// Usually stdout closes because the process exited; prefer reporting
	// its exit status.
	select {
	case <-t.exited:
		if t.exitErr != nil {
			err = t.exitErr
		} else {
			err = errors.New("exited")
		}
	case <-time.After(500 * time.Millisecond):
	}
	t.markDead(notRunning(t.name, err))
}

func (t *stdioTransport) stderrLoop() {
	sc := bufio.NewScanner(t.stderr)
	sc.Buffer(make([]byte, 4096), 64*1024)
	for sc.Scan() {
		t.log.Debug("mcp server stderr", "server", t.name, "line", sc.Text())
	}
}

func (t *stdioTransport) write(ctx context.Context, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		t.stdin.SetWriteDeadline(dl)
	} else {
		t.stdin.SetWriteDeadline(time.Time{})
	}
	_, err = t.stdin.Write(b)
	return err
}

func (t *stdioTransport) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := t.err(); err != nil {
		return nil, err
	}
	id := t.nextID.Add(1)
	ch := make(chan rpcMessage, 1)
	t.mu.Lock()
	t.pending[id] = ch
	t.mu.Unlock()
	forget := func() {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
	}
	if err := t.write(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		forget()
		if derr := t.err(); derr != nil {
			return nil, derr
		}
		return nil, fmt.Errorf("mcp server %s: write: %w", t.name, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		forget()
		// Best effort: tell the server to stop working on it.
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = t.notify(cctx, "notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
		cancel()
		return nil, fmt.Errorf("mcp server %s: %s: %w", t.name, method, ctx.Err())
	case <-t.dead:
		forget()
		return nil, t.err()
	}
}

func (t *stdioTransport) notify(ctx context.Context, method string, params any) error {
	if err := t.err(); err != nil {
		return err
	}
	return t.write(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// close shuts the child down: stdin is closed and SIGTERM sent; if it has not
// exited after killGrace it is killed.
func (t *stdioTransport) close() error {
	t.closeOnce.Do(func() {
		t.markDead(fmt.Errorf("mcp server %s is %w (closed)", t.name, errNotRunning))
		t.stdin.Close()
		select {
		case <-t.exited:
		default:
			_ = t.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-t.exited:
			case <-time.After(t.killGrace):
				t.log.Warn("mcp server did not exit after SIGTERM; killing", "server", t.name)
				_ = t.cmd.Process.Kill()
				<-t.exited
			}
		}
		t.stdout.Close()
		t.stderr.Close()
	})
	return nil
}
