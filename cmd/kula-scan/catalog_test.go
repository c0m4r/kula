package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// emittedIDs parses the package's non-test sources and returns every string
// literal passed as the finding ID to finding(), or to a helper that hands its
// first (string) parameter on to finding() as the ID, such as fuzzFinding or
// the header check's want closure, mapped to where it appears.
func emittedIDs(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}

	// Every named function, method and function literal bound to a name,
	// with the name of its first parameter when that one is a string.
	type function struct {
		name, param string
		body        *ast.BlockStmt
	}
	firstStringParam := func(ft *ast.FuncType) string {
		if ft.Params == nil || len(ft.Params.List) == 0 || len(ft.Params.List[0].Names) == 0 {
			return ""
		}
		if ident, ok := ft.Params.List[0].Type.(*ast.Ident); ok && ident.Name == "string" {
			return ft.Params.List[0].Names[0].Name
		}
		return ""
	}
	var functions []function
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Body != nil {
					functions = append(functions, function{n.Name.Name, firstStringParam(n.Type), n.Body})
				}
			case *ast.AssignStmt:
				for i, rhs := range n.Rhs {
					lit, isLit := rhs.(*ast.FuncLit)
					if !isLit || i >= len(n.Lhs) {
						continue
					}
					if ident, ok := n.Lhs[i].(*ast.Ident); ok {
						functions = append(functions, function{ident.Name, firstStringParam(lit.Type), lit.Body})
					}
				}
			}
			return true
		})
	}

	callee := func(call *ast.CallExpr) string {
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			return fn.Name
		case *ast.SelectorExpr:
			return fn.Sel.Name
		}
		return ""
	}

	// A function forwards an ID when it passes its first parameter as the
	// first argument of finding() or of another forwarder.
	forwards := map[string]bool{"finding": true}
	for changed := true; changed; {
		changed = false
		for _, fn := range functions {
			if fn.param == "" || forwards[fn.name] {
				continue
			}
			ast.Inspect(fn.body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 || !forwards[callee(call)] {
					return true
				}
				if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == fn.param {
					forwards[fn.name] = true
					changed = true
				}
				return true
			})
		}
	}

	ids := map[string]string{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 || !forwards[callee(call)] {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			id, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			ids[id] = fset.Position(lit.Pos()).String()
			return true
		})
	}
	return ids
}

func TestCatalogCoversEveryEmittedID(t *testing.T) {
	emitted := emittedIDs(t)
	if len(emitted) < 20 {
		t.Fatalf("found only %d emitted IDs; the source scan is broken: %v", len(emitted), emitted)
	}
	for id, where := range emitted {
		if _, ok := lookupID(id); !ok {
			t.Errorf("%s: finding ID %q is not in the catalog", where, id)
		}
	}
	for _, e := range catalog {
		if _, ok := emitted[e.ID]; !ok {
			t.Errorf("catalog entry %s: no finding() call emits it", e.ID)
		}
	}
}

func TestCatalogEntriesAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range catalog {
		if seen[strings.ToLower(e.ID)] {
			t.Errorf("%s is in the catalog twice", e.ID)
		}
		seen[strings.ToLower(e.ID)] = true
		if e.ID != strings.ToUpper(e.ID) || !strings.Contains(e.ID, "-") {
			t.Errorf("%s: an ID is upper case with a hyphen", e.ID)
		}
		if e.Category == "" || e.Category != strings.ToLower(e.Category) || e.Title == "" || e.Description == "" {
			t.Errorf("%s: incomplete entry %+v", e.ID, e)
		}
	}
	// Category names and IDs share the -only/-skip namespace.
	for _, c := range categories() {
		if _, ok := lookupID(c); ok {
			t.Errorf("category %q is also a finding ID", c)
		}
	}
}

// TestRegistryDeclaresEveryCatalogID: every finding ID but discovery's is
// declared by exactly one registry check, and the IDs of one check agree on
// category and mode, which the runner reads from its first ID.
func TestRegistryDeclaresEveryCatalogID(t *testing.T) {
	declared := map[string]int{}
	for i, c := range allChecks() {
		if len(c.ids) == 0 {
			t.Errorf("check %d declares no finding IDs", i)
			continue
		}
		first := c.entry()
		for _, id := range c.ids {
			e, ok := lookupID(id)
			if !ok {
				t.Errorf("check %d declares %s, which is not in the catalog", i, id)
				continue
			}
			if e.Category != first.Category || e.Mode != first.Mode {
				t.Errorf("check %d mixes %s (%s, %s) with %s (%s, %s)", i, id, e.Category, e.Mode, first.ID, first.Category, first.Mode)
			}
			declared[id]++
		}
	}
	for _, e := range catalog {
		switch {
		case e.Category == "discovery":
			if declared[e.ID] != 0 {
				t.Errorf("%s: discovery findings come from discover(), not from a registry check", e.ID)
			}
		case declared[e.ID] != 1:
			t.Errorf("%s is declared by %d registry checks, want 1", e.ID, declared[e.ID])
		}
	}
}

func TestUnknownFindingIDStillRenders(t *testing.T) {
	f := finding("NOPE-404", SevHigh, StatusFail, "detail")
	if f.Title != "NOPE-404" || f.Category != "unknown" {
		t.Errorf("an uncatalogued ID got title %q, category %q", f.Title, f.Category)
	}
	f = finding("PROM-001", SevMedium, StatusWarn, "")
	if f.Title != "Prometheus token enforced" || f.Category != "metrics" {
		t.Errorf("PROM-001 got title %q, category %q", f.Title, f.Category)
	}
}

func TestListPrintsTheCatalog(t *testing.T) {
	var out bytes.Buffer
	if err := writeCatalog(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, e := range catalog {
		if !strings.Contains(text, e.ID) || !strings.Contains(text, e.Title) {
			t.Errorf("-list does not show %s %q", e.ID, e.Title)
		}
	}
}

// TestSkippedChecksKeepTheirTitles: a check left out for want of -aggressive
// or -fuzz reports each of its IDs under the catalog title, not a generic one.
func TestSkippedChecksKeepTheirTitles(t *testing.T) {
	findings, _ := scanWithLedger(t, false, nil)
	for _, id := range []string{"AUTH-006", "RATE-LOGIN", "DOS-SLOWLORIS", "FUZZ-LIVENESS"} {
		f, ok := byID(findings, id)
		if !ok || f.Status != StatusSkip || f.Title != catalogEntryFor(id).Title {
			t.Errorf("%s: got %+v, want a SKIP titled %q", id, f, catalogEntryFor(id).Title)
		}
	}
}
