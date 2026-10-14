package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/fake"
)

type fileState struct {
	size  int64
	mtime time.Time
	mode  fs.FileMode
}

func snapshotDir(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		out[p] = fileState{size: fi.Size(), mtime: fi.ModTime(), mode: fi.Mode()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNothingWritesDataDirAfterClose is the regression test for a shutdown
// race: background workers (the session sweep runs a query immediately at
// start) outlived Close and reopened SQLite connections after the store
// was closed, creating bosun.db-wal/-shm in the data dir while t.TempDir's
// cleanup was removing it. Close must wait for every background writer.
func TestNothingWritesDataDirAfterClose(t *testing.T) {
	for i := range 15 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			dir := t.TempDir()
			data := filepath.Join(dir, "data")
			cfg, err := config.Parse(fmt.Appendf(nil, `
daemon: { listen: "127.0.0.1:0", data_dir: %q, master_key: file }
llm: { default: fake/m, providers: { fake: {} } }
integrations:
  notes: {}
  email: { poll_interval: 30s }
  mcp:
    servers:
      - { name: broken, command: ["/nonexistent/mcp-server"] }
`, data))
			if err != nil {
				t.Fatal(err)
			}
			reg := llm.NewRegistry()
			if err := reg.Register(fake.New("fake")); err != nil {
				t.Fatal(err)
			}
			// The parent context stays alive until after Close, as in the
			// tests that used to flake: Close itself must stop the workers.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d, err := newDaemon(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
				deps{LLM: reg, Argon2: cheapArgon(), RoutineTick: time.Millisecond, ApprovalSweep: time.Millisecond})
			if err != nil {
				t.Fatalf("newDaemon: %v", err)
			}
			if i%3 == 1 {
				// Serve briefly too, so the HTTP side is part of the run.
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				sctx, scancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- d.Serve(sctx, ln) }()
				time.Sleep(5 * time.Millisecond)
				scancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			d.Close()
			d.Close() // idempotent
			before := snapshotDir(t, data)
			time.Sleep(150 * time.Millisecond)
			after := snapshotDir(t, data)
			if !maps.Equal(before, after) {
				for p, st := range after {
					if b, ok := before[p]; !ok || b != st {
						t.Errorf("changed after Close: %s", p)
					}
				}
				for p := range before {
					if _, ok := after[p]; !ok {
						t.Errorf("removed after Close: %s", p)
					}
				}
			}
			for p := range after {
				if filepath.Base(p) == "bosun.db-wal" || filepath.Base(p) == "bosun.db-shm" {
					t.Errorf("SQLite side file left after Close: %s", p)
				}
			}
			if err := os.RemoveAll(data); err != nil {
				t.Fatal(err)
			}
		})
	}
}
