package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// TestExpectKeysNameTheirFindings: each -expect key lists exactly the
// findings the scanner's source tags with expecting(key), so the startup
// check that refuses an unjudged expectation knows every finding that could
// judge it. A tag with a literal key on a finding whose ID the test cannot
// read is an error: the tag must sit on a finding("ID", …) expression.
func TestExpectKeysNameTheirFindings(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	tagged := map[string][]string{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "expecting" || len(call.Args) != 1 {
				return true
			}
			key, ok := stringLiteral(call.Args[0])
			if !ok {
				// A key that is no literal comes from expectKeys itself, as
				// a finding judging a key the run cannot judge does.
				return true
			}
			id := taggedFindingID(sel.X)
			if id == "" {
				t.Errorf("%s: an expecting(…) call whose key or finding ID the test cannot read", fset.Position(call.Pos()))
				return true
			}
			if !slices.Contains(tagged[key], id) {
				tagged[key] = append(tagged[key], id)
			}
			return true
		})
	}
	if len(tagged) == 0 {
		t.Fatal("found no expecting(…) call; is the test running in the package directory?")
	}

	declared := map[string]bool{}
	for _, k := range expectKeys {
		declared[k.key] = true
		got := slices.Clone(tagged[k.key])
		want := slices.Clone(k.ids)
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("-expect %s lists %v, but the checks tag %v with it", k.key, want, got)
		}
		for _, id := range k.ids {
			if _, ok := lookupID(id); !ok {
				t.Errorf("-expect %s lists %s, which is not in the catalog", k.key, id)
			}
		}
	}
	for key, ids := range tagged {
		if !declared[key] {
			t.Errorf("%v are tagged with expecting(%q), which is no -expect key", ids, key)
		}
	}
}

// taggedFindingID returns the ID of the finding expression x builds:
// finding("ID", …), followed by any chain of method calls.
func taggedFindingID(x ast.Expr) string {
	for {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return ""
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name != "finding" || len(call.Args) == 0 {
				return ""
			}
			id, _ := stringLiteral(call.Args[0])
			return id
		case *ast.SelectorExpr:
			x = fun.X
		default:
			return ""
		}
	}
}

// stringLiteral returns the value of a string literal expression.
func stringLiteral(x ast.Expr) (string, bool) {
	lit, ok := x.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// TestUnjudgedExpectationsAreUsageErrors: an -expect key all of whose
// findings the run leaves out, by -only, -skip, -ignore or a mode it does not
// enable, stops the scan before it starts, naming the key, its findings and
// why each is out. One finding left in is enough.
func TestUnjudgedExpectationsAreUsageErrors(t *testing.T) {
	var requests atomic.Int64
	mock := secureMock()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mock.ServeHTTP(w, r)
	}))
	defer ts.Close()

	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"-only", "headers", "-expect", "auth"},
			[]string{"-expect auth is judged by AUTH-000, which this run does not judge (AUTH-000 is not selected by -only)"}},
		{[]string{"-skip", "HDR-008", "-expect", "hsts"},
			[]string{"-expect hsts is judged by HDR-008, which this run does not judge (HDR-008 is skipped by -skip)"}},
		{[]string{"-expect", "auth", "-ignore", "auth-000"},
			[]string{"(AUTH-000 is ignored by -ignore); select its findings, or drop auth from -expect"}},
		{[]string{"-skip", "headers", "-expect", "frame-protection"},
			[]string{"-expect frame-protection is judged by HDR-002, HDR-007, none of which this run judges (HDR-002 is skipped by -skip; HDR-007 is skipped by -skip)"}},
		{[]string{"-only", "headers,tls", "-skip", "TLS-001,TLS-004", "-expect", "tls,auth,hsts"},
			[]string{"-expect auth is judged by AUTH-000",
				"(DISC-002 is not selected by -only; TLS-001 is skipped by -skip; TLS-004 is skipped by -skip)"}},
	} {
		requests.Store(0)
		code, stdout, stderr := runCaptured(t, append(c.args, ts.URL)...)
		ok := code == exitUsage && stdout == "" && strings.HasPrefix(stderr, "error: -expect ")
		for _, want := range c.want {
			ok = ok && strings.Contains(stderr, want)
		}
		if !ok {
			t.Errorf("%v: exit %d, want %d with no report and %q on stderr\nstdout:\n%s\nstderr:\n%s",
				c.args, code, exitUsage, c.want, stdout, stderr)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("%v: the target got %d requests from a scan that should not start", c.args, n)
		}
	}
	// One finding left to judge each key is enough.
	for _, args := range [][]string{
		{"-only", "TLS-001", "-expect", "tls"},
		{"-only", "HDR-007,AUTH-000", "-expect", "frame-protection,auth"},
		{"-skip", "HDR-002", "-expect", "frame-protection"},
		{"-ignore", "HDR-002", "-expect", "frame-protection"},
	} {
		code, _, stderr := runCaptured(t, append(args, ts.URL)...)
		if code == exitUsage {
			t.Errorf("%v: a usage error with a finding left to judge the expectation:\n%s", args, stderr)
		}
	}

	// A key whose findings all come from a mode the run does not enable.
	saved := expectKeys
	t.Cleanup(func() { expectKeys = saved })
	expectKeys = append(slices.Clone(saved), expectKey{"enumeration", "", []string{"AUTH-006", "FUZZ-BODY"}})
	for _, c := range []struct {
		aggressive, fuzz bool
		want             string
	}{
		{false, false, "(AUTH-006 runs only with -aggressive; FUZZ-BODY runs only with -fuzz)"},
		{true, false, ""},
		{false, true, ""},
	} {
		s, err := NewScanner(Options{Target: ts.URL, Expect: []string{"enumeration"}, Aggressive: c.aggressive, Fuzz: c.fuzz})
		if err != nil {
			t.Fatal(err)
		}
		err = s.unjudgedExpectations(nil, nil)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("aggressive %v, fuzz %v: %v", c.aggressive, c.fuzz, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("aggressive %v, fuzz %v: %v, want %q", c.aggressive, c.fuzz, err, c.want)
		}
	}
}
