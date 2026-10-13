package integrations

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/dperedo/bosun"

// forbiddenExact are imports no integration may use.
var forbiddenExact = []string{"os/exec"}

// forbiddenPrefix are module packages (and their subpackages) no integration
// may use.
var forbiddenPrefix = []string{
	modulePath + "/internal/store",
	modulePath + "/internal/auth",
}

const integrationsPkg = modulePath + "/internal/integrations"

// checkImport returns a non-empty reason if a file belonging to integration
// own (the first path segment under internal/integrations; "" for files in
// the root package) may not import imp.
func checkImport(own, imp string) string {
	for _, f := range forbiddenExact {
		if imp == f {
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
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, is := range f.Imports {
			imp, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: bad import %s", path, is.Path.Value)
			}
			if why := checkImport(own, imp); why != "" {
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
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/integrations/email/imap"
)`)
	write("email/imap/imap.go", `package imap
import "github.com/dperedo/bosun/internal/integrations/email"`)
	write("email/bad.go", `package email
import (
	"os/exec"
	st "github.com/dperedo/bosun/internal/store"
	"github.com/dperedo/bosun/internal/auth/vault"
	"github.com/dperedo/bosun/internal/integrations/calendar"
)`)
	write("calendar/bad_test.go", `package calendar
import _ "github.com/dperedo/bosun/internal/integrations/notes/sub"`)
	write("notes/x.go", `package notes
import "github.com/dperedo/bosun/internal/integrations"`)
	write("root.go", `package integrations
import "github.com/dperedo/bosun/internal/integrations/notes"`)

	violations, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(violations, "\n")
	for _, want := range []string{
		"bad.go:3:2: imports os/exec",
		"imports github.com/dperedo/bosun/internal/store",
		"imports github.com/dperedo/bosun/internal/auth/vault",
		"bad.go:6:2: imports another integration (calendar)",
		"bad_test.go:2:8: imports another integration (notes)",
		"x.go:2:8: imports the integrations root package",
		"root.go:2:8: imports another integration (notes)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing violation %q in:\n%s", want, joined)
		}
	}
	if len(violations) != 7 {
		t.Errorf("got %d violations, want 7:\n%s", len(violations), joined)
	}
}
