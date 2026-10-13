package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyDispatcherInvokesHandlers enforces ARCHITECTURE.md's rule that the
// dispatcher is the only path from the model to a Tool Handler. It parses
// every Go file under internal/ and cmd/ and fails on any use of a
// `.Handler` field as a value: a call `x.Handler(ctx, in)`, or `x.Handler`
// assigned, passed, or returned (which would let it be called elsewhere).
//
// Allowed: internal/dispatch itself, and _test.go files under
// internal/integration and internal/integrations (integrations unit-test
// their own handlers directly). References through an imported package
// (e.g. http.Handler as a type) and nil comparisons (the registry's
// validation) are not invocations and are ignored.
func TestOnlyDispatcherInvokesHandlers(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if allowedHandlerUse(rel) {
				return nil
			}
			violations = append(violations, scanHandlerUses(t, path, rel)...)
			return nil
		})
		if err != nil && !strings.Contains(err.Error(), "no such file") {
			t.Fatal(err)
		}
	}
	for _, v := range violations {
		t.Errorf("tool Handler used outside internal/dispatch: %s", v)
	}
}

func allowedHandlerUse(rel string) bool {
	if strings.HasPrefix(rel, "internal/dispatch/") {
		return true
	}
	isTest := strings.HasSuffix(rel, "_test.go")
	return isTest && (strings.HasPrefix(rel, "internal/integration/") || strings.HasPrefix(rel, "internal/integrations/"))
}

func scanHandlerUses(t *testing.T, path, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	imports := map[string]bool{}
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = true
	}
	isHandlerSel := func(e ast.Expr) bool {
		sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Handler" {
			return false
		}
		if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] {
			return false // pkg.Handler: a type or package-level identifier
		}
		return true
	}
	var out []string
	report := func(n ast.Node, what string) {
		out = append(out, rel+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+": "+what)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			// x.Handler(ctx, in): a tool handler call has exactly two args.
			// (Zero-arg x.Handler() is e.g. an http server's handler getter.)
			if isHandlerSel(x.Fun) && len(x.Args) == 2 {
				report(x, "call")
			}
			for _, a := range x.Args {
				if isHandlerSel(a) {
					report(a, "passed as argument")
				}
			}
		case *ast.AssignStmt:
			for _, r := range x.Rhs {
				if isHandlerSel(r) {
					report(r, "assigned")
				}
			}
		case *ast.ValueSpec:
			for _, v := range x.Values {
				if isHandlerSel(v) {
					report(v, "assigned")
				}
			}
		case *ast.ReturnStmt:
			for _, r := range x.Results {
				if isHandlerSel(r) {
					report(r, "returned")
				}
			}
		case *ast.KeyValueExpr:
			if isHandlerSel(x.Value) {
				report(x.Value, "copied into a composite literal")
			}
		case *ast.CompositeLit:
			for _, el := range x.Elts {
				if isHandlerSel(el) {
					report(el, "copied into a composite literal")
				}
			}
		}
		return true
	})
	return out
}

// TestChokepointScannerDetects guards the scanner itself against silently
// passing.
func TestChokepointScannerDetects(t *testing.T) {
	src := `package x
import "net/http"
var _ http.Handler
func f(t T, ctx C) {
	t.Handler(ctx, nil)
	h := t.Handler
	g(t.Handler)
	_ = T{Handler: s.Handler()}
	if t.Handler == nil {}
}
func r(t T) any { return t.Handler }
`
	path := filepath.Join(t.TempDir(), "x.go")
	if err := writeFile(path, src); err != nil {
		t.Fatal(err)
	}
	got := scanHandlerUses(t, path, "x.go")
	if len(got) != 4 {
		t.Errorf("violations = %v, want 4 (call, assign, arg, return)", got)
	}
}
