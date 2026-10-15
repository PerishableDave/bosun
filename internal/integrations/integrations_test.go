package integrations

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/perishabledave/bosun"

// forbiddenExact are imports no integration may use, except as allowed by
// exactAllow.
var forbiddenExact = []string{"os/exec"}

// exactAllow names, per forbidden import, the integration directories
// (relative to internal/integrations) allowed to use it.
//
// os/exec: the MCP bridge (internal/integrations/mcp) must spawn stdio MCP
// servers, which are subprocesses by definition; that is the point of the
// bridge, and the external server is already a separate process. The
// exemption covers that one package directory only: not its subpackages
// (e.g. mcp/mcptest) and not any other integration. The bridge gives
// children a minimal environment rather than the daemon's.
//
// os/exec: the browser integration (internal/integrations/browser) must
// launch Chromium, which is a separate process by definition; chromedp's
// allocator starts it and the integration only uses *exec.Cmd to put the
// browser in its own process group (so stopping a session kills its helper
// processes). Same scope as the MCP exemption: that one package directory,
// not its subpackages. The browser gets a minimal environment, its own
// profile directory, a pipe (not a TCP port) for DevTools, and an
// SSRF-guarded proxy; it executes nothing else.
var exactAllow = map[string][]string{"os/exec": {"mcp", "browser"}}

// forbiddenPrefix are module packages (and their subpackages) no integration
// may use.
var forbiddenPrefix = []string{
	modulePath + "/internal/store",
	modulePath + "/internal/auth",
	modulePath + "/internal/vault",    // credentials arrive via CredentialSource only
	modulePath + "/internal/dispatch", // integrations never invoke tools themselves
}

const integrationsPkg = modulePath + "/internal/integrations"

// checkImport returns a non-empty reason if a file belonging to integration
// own (the first path segment under internal/integrations; "" for files in
// the root package), located in dir (slash-separated, relative to
// internal/integrations), may not import imp.
func checkImport(own, dir, imp string) string {
	for _, f := range forbiddenExact {
		if imp == f {
			if slices.Contains(exactAllow[f], dir) {
				continue
			}
			return "imports " + imp
		}
	}
	for _, p := range forbiddenPrefix {
		if imp == p || strings.HasPrefix(imp, p+"/") {
			return "imports " + imp
		}
	}
	if imp == integrationsPkg {
		if own != "" {
			return "imports the integrations root package"
		}
		return ""
	}
	if rest, ok := strings.CutPrefix(imp, integrationsPkg+"/"); ok {
		target, _, _ := strings.Cut(rest, "/")
		if target != own {
			return fmt.Sprintf("imports another integration (%s)", target)
		}
	}
	return ""
}

// scan walks root (the internal/integrations directory) and returns one
// message per forbidden import.
func scan(root string) ([]string, error) {
	var violations []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		own := ""
		if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) > 1 {
			own = parts[0]
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, is := range f.Imports {
			imp, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: bad import %s", path, is.Path.Value)
			}
			if why := checkImport(own, dir, imp); why != "" {
				violations = append(violations, fmt.Sprintf("%s: %s", fset.Position(is.Pos()), why))
			}
		}
		return nil
	})
	return violations, err
}

// TestIntegrationImports is the compile-time substitute for process
// isolation described in ARCHITECTURE.md.
func TestIntegrationImports(t *testing.T) {
	violations, err := scan(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// TestGuardCatchesViolations makes sure the guard itself works.
func TestGuardCatchesViolations(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("email/ok.go", `package email
import (
	"context"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/integrations/email/imap"
)`)
	write("email/imap/imap.go", `package imap
import "github.com/perishabledave/bosun/internal/integrations/email"`)
	write("email/bad.go", `package email
import (
	"os/exec"
	st "github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/auth/vault"
	"github.com/perishabledave/bosun/internal/integrations/calendar"
)`)
	write("email/worse.go", `package email
import (
	"github.com/perishabledave/bosun/internal/vault"
	"github.com/perishabledave/bosun/internal/dispatch"
)`)
	write("calendar/bad_test.go", `package calendar
import _ "github.com/perishabledave/bosun/internal/integrations/notes/sub"`)
	write("notes/x.go", `package notes
import "github.com/perishabledave/bosun/internal/integrations"`)
	write("root.go", `package integrations
import "github.com/perishabledave/bosun/internal/integrations/notes"`)
	// The MCP bridge may spawn processes; its subpackages and others may not.
	write("mcp/stdio.go", `package mcp
import "os/exec"`)
	write("mcp/sub/sub.go", `package sub
import "os/exec"`)
	write("mcpx/x.go", `package mcpx
import "os/exec"`)
	// So may the browser integration (it launches Chromium); not its
	// subpackages.
	write("browser/launch.go", `package browser
import "os/exec"`)
	write("browser/sub/sub.go", `package sub
import "os/exec"`)

	violations, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(violations, "\n")
	for _, want := range []string{
		"bad.go:3:2: imports os/exec",
		"imports github.com/perishabledave/bosun/internal/store",
		"imports github.com/perishabledave/bosun/internal/auth/vault",
		"bad.go:6:2: imports another integration (calendar)",
		"bad_test.go:2:8: imports another integration (notes)",
		"x.go:2:8: imports the integrations root package",
		"root.go:2:8: imports another integration (notes)",
		"worse.go:3:2: imports github.com/perishabledave/bosun/internal/vault",
		"worse.go:4:2: imports github.com/perishabledave/bosun/internal/dispatch",
		"sub.go:2:8: imports os/exec",
		"x.go:2:8: imports os/exec",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing violation %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "stdio.go") {
		t.Errorf("mcp/stdio.go must be allowed to import os/exec:\n%s", joined)
	}
	if strings.Contains(joined, "launch.go") {
		t.Errorf("browser/launch.go must be allowed to import os/exec:\n%s", joined)
	}
	if !strings.Contains(joined, "browser/sub/sub.go") && !strings.Contains(joined, filepath.Join("browser", "sub", "sub.go")) {
		t.Errorf("browser/sub must not be allowed to import os/exec:\n%s", joined)
	}
	if len(violations) != 12 {
		t.Errorf("got %d violations, want 12:\n%s", len(violations), joined)
	}
}
