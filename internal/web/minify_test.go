package web

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"kula/internal/config"
)

func TestMinifyJS(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"comments", "a = 1; // note\nb = 2; /* note */ c()", "a=1;b=2;c()"},
		{"block comment with line break keeps ASI", "a /*\n*/ b", "a\nb"},
		{"statements without semicolons", "let a = 1\nlet b = 2", "let a=1\nlet b=2"},
		{"restricted return", "return\nx", "return\nx"},
		{"restricted postfix", "a\n++b", "a\n++b"},
		{"line break before parenthesis", "a = b\n(c)", "a=b\n(c)"},
		{"method chain", "foo()\n  .bar()\n  .baz()", "foo().bar().baz()"},
		{"optional chain", "a\n  ?.b", "a?.b"},
		{"block body", "const f = (a, b) => {\n  return a + b;\n};", "const f=(a,b)=>{return a+b;};"},
		{"call arguments", "f(\n  a,\n  b\n)", "f(a,b)"},
		{"class fields", "class A {\n  x = 1\n  #y = 2\n  static z\n  m() {}\n}", "class A{x=1\n#y=2\nstatic z\nm(){}}"},
		{"keywords", "return typeof x === 'string'", "return typeof x==='string'"},
		{"unary plus", "a + +b", "a+ +b"},
		{"unary minus", "a - -b", "a- -b"},
		{"prefix increment", "a + ++b", "a+ ++b"},
		{"postfix increment", "a++ + b", "a++ +b"},
		{"html comment open", "a < !b", "a< !b"},
		{"html comment close", "a-- > b", "a-- >b"},
		{"number member", "1 .toString()", "1 .toString()"},
		{"decimal member", "x = 1.5 .toFixed(1)", "x=1.5 .toFixed(1)"},
		{"conditional decimal", "a ? .5 : b", "a?.5:b"},
		{"division", "x = a / b / c", "x=a/b/c"},
		{"regex keeps spaces", "s.replace(/ +/g, ' ')", "s.replace(/ +/g,' ')"},
		{"regex after if head", "if (x) /re/.test(y)", "if(x)/re/.test(y)"},
		{"regex after call is division", "f(x) / 2", "f(x)/2"},
		{"regex quotes and slash in class", "const r = /[`'\"/]/g; x = 1", "const r=/[`'\"/]/g;x=1"},
		{"regex flags before word", "x = /re/g instanceof RegExp", "x=/re/g instanceof RegExp"},
		{"division before regex", "a / /re/.source.length", "a/ /re/.source.length"},
		{"regex before division", "x = /re/ / 2", "x=/re/ /2"},
		{"strings keep comment markers", "'// not' + \"/* nor */\"", "'// not'+\"/* nor */\""},
		{"string line continuation", "'a\\\nb' + c", "'a\\\nb'+c"},
		{"template substitutions", "`a ${ b + `c ${ d }` } e`", "`a ${b+`c ${d}`} e`"},
		{"template object literal", "`${ {a: 1}.a }`", "`${{a:1}.a}`"},
		{"template keeps text", "`  x\n  // y\n`", "`  x\n  // y\n`"},
		{"template substitution regex", "`${ /a b/.source }`", "`${/a b/.source}`"},
		{"line separator", "a\u2028b", "a\nb"},
		{"non-ascii identifier", "const zażółć = 1", "const zażółć=1"},
		{"private in", "#x in obj", "#x in obj"},
		{"bigint and exponent", "x = 1e+5 + 0x1f - 10n", "x=1e+5+0x1f-10n"},
		{"spread", "f(... args)", "f(...args)"},
		{"arrow on same line", "a = (b) =>\n  b", "a=(b)=>b"},
		{"async arrow", "async\n(x) => x", "async\n(x)=>x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := minifyJS([]byte(tt.in))
			if err != nil {
				t.Fatalf("minifyJS(%q) error: %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Errorf("minifyJS(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMinifyJSRejectsInput(t *testing.T) {
	for _, in := range []string{
		"x = 'abc",
		"x = 'abc\ny'",
		"x = `abc",
		"x = `${a",
		"x = 1 /* note",
		"x = /abc\n/",
		"x = /[/",
		". ..", // would join to "..." (a spread); rejected by the token re-check
	} {
		if got, err := minifyJS([]byte(in)); err == nil {
			t.Errorf("minifyJS(%q) = %q, want an error", in, got)
		}
	}
}

func TestMinifyCSS(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"declarations", "a { color: red ; }", "a{color:red}"},
		{"space before colon kept", "a { color : red }", "a{color :red}"},
		{"combinators", ".a .b > .c , .d ~ .e + .f { }", ".a .b>.c,.d~.e + .f{}"},
		{"calc operators", "a { width: calc(100% - 2rem); top: calc(100% + 5px) }", "a{width:calc(100% - 2rem);top:calc(100% + 5px)}"},
		{"media query", "@media screen and (max-width: 600px) {\n  a { b: c }\n}", "@media screen and (max-width:600px){a{b:c}}"},
		{"descendant pseudo-class", ".a :hover, a:not(.b) .c { }", ".a :hover,a:not(.b) .c{}"},
		{"comments", "/* x */ a { /* y */ b: c; }", "a{b:c}"},
		{"strings", "a::before { content: \" { ; } /* */ \" }", "a::before{content:\" { ; } /* */ \"}"},
		{"escaped colon", ".a\\: .b { }", ".a\\: .b{}"},
		{"quoted url", "@font-face { src: url('x y.ttf') format('truetype'); }", "@font-face{src:url('x y.ttf') format('truetype')}"},
		{"unquoted url", "a { background: url( data:image/svg+xml;x/*y*/z ) }", "a{background:url(data:image/svg+xml;x/*y*/z)}"},
		{"empty custom property", "a { --x: ; }", "a{--x: }"},
		{"important", "a { b: c !important; }", "a{b:c!important}"},
		{"value lists", "a { font: 12px 'Inter', sans-serif; color: rgba(0, 0, 0, .5) }", "a{font:12px 'Inter',sans-serif;color:rgba(0,0,0,.5)}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := minifyCSS([]byte(tt.in))
			if err != nil {
				t.Fatalf("minifyCSS(%q) error: %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Errorf("minifyCSS(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMinifyCSSRejectsUnterminatedInput(t *testing.T) {
	for _, in := range []string{"a { b: c } /* note", "a::before { content: \"x }", "a { content: 'x\n' }"} {
		if got, err := minifyCSS([]byte(in)); err == nil {
			t.Errorf("minifyCSS(%q) = %q, want an error", in, got)
		}
	}
}

// embeddedMinifiable returns every embedded path that staticMinifier handles.
func embeddedMinifiable(t *testing.T) []string {
	t.Helper()
	var paths []string
	err := fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && staticMinifier(path) != nil {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk static: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no minifiable static assets found")
	}
	return paths
}

// TestEmbeddedAssetsMinify checks every served stylesheet and script: none
// falls back to its unminified form, minification only drops whitespace and
// comments, and a second pass changes nothing.
func TestEmbeddedAssetsMinify(t *testing.T) {
	cssComment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssSpace := regexp.MustCompile(`\s+`)
	assets := minifiedStatic()
	for _, path := range embeddedMinifiable(t) {
		src, err := staticFS.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		min, ok := assets[path]
		if !ok {
			t.Errorf("%s is served unminified", path)
			continue
		}
		again, err := staticMinifier(path)(min)
		if err != nil || !bytes.Equal(again, min) {
			t.Errorf("%s: minifying twice is not stable (err %v)", path, err)
		}
		if strings.HasSuffix(path, ".css") {
			strip := func(b []byte) string {
				s := cssSpace.ReplaceAllString(cssComment.ReplaceAllString(string(b), ""), "")
				return strings.ReplaceAll(s, ";}", "}")
			}
			if strip(src) != strip(min) {
				t.Errorf("%s: minified stylesheet differs beyond whitespace and comments", path)
			}
			continue
		}
		want, err := lexJS(string(src))
		if err != nil {
			t.Fatalf("lexJS(%s): %v", path, err)
		}
		if got, err := lexJS(string(min)); err != nil || !sameJSTokens(got, want) {
			t.Errorf("%s: minified script lexes to different tokens (err %v)", path, err)
		}
	}
}

// TestMinifiedScriptsParseInNode syntax-checks every minified script with
// node, when it is installed.
func TestMinifiedScriptsParseInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping minified script syntax check")
	}
	dir := t.TempDir()
	assets := minifiedStatic()
	for _, path := range embeddedMinifiable(t) {
		if !strings.HasSuffix(path, ".js") {
			continue
		}
		// Dashboard modules use import/export; game.js is a classic script.
		name := strings.ReplaceAll(strings.TrimPrefix(path, "static/"), "/", "_")
		if strings.HasPrefix(path, "static/js/app/") {
			name = strings.TrimSuffix(name, ".js") + ".mjs"
		} else {
			name = strings.TrimSuffix(name, ".js") + ".cjs"
		}
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, assets[path], 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", file).CombinedOutput(); err != nil {
			t.Errorf("minified %s does not parse: %v\n%s", path, err, out)
		}
	}
}

func TestStaticAssetsServedMinified(t *testing.T) {
	for _, minify := range []bool{true, false} {
		cfg := config.WebConfig{UI: true, MinifyAssets: minify}
		s := NewServer(cfg, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})
		for _, path := range []string{"js/app/main.js", "style.css", "js/chartjs/chart.umd.min.js"} {
			rec := httptest.NewRecorder()
			s.handleStatic(rec, httptest.NewRequest(http.MethodGet, "/"+path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("minify=%v GET /%s = %d", minify, path, rec.Code)
			}
			body := rec.Body.Bytes()
			src, err := staticFS.ReadFile("static/" + path)
			if err != nil {
				t.Fatal(err)
			}
			wantMinified := minify && !strings.Contains(path, ".min.")
			if wantMinified && len(body) >= len(src) {
				t.Errorf("minify=%v /%s served %d bytes, want fewer than the %d-byte source", minify, path, len(body), len(src))
			}
			if !wantMinified && !bytes.Equal(body, src) {
				t.Errorf("minify=%v /%s did not serve the embedded source", minify, path)
			}
			if !strings.HasSuffix(path, ".js") {
				continue
			}
			sum := sha512.Sum384(body)
			if want := "sha384-" + base64.StdEncoding.EncodeToString(sum[:]); s.sriHashes[path] != want {
				t.Errorf("minify=%v SRI for %s = %q, want the hash of the served bytes %q", minify, path, s.sriHashes[path], want)
			}
		}
	}
}

// FuzzMinifyJS asserts the JavaScript minifier never panics and, whenever it
// accepts an input, only drops whitespace and comments: the output lexes to
// the same tokens, and minifying it again changes nothing.
func FuzzMinifyJS(f *testing.F) {
	for _, s := range []string{
		"a = 1; // note\nb = 2",
		"return\nx",
		"a\n++b",
		"a + +b - -c",
		"x = a / b / c; s.replace(/ +/g, ' ')",
		"if (x) /re/.test(y)",
		"`a ${ b + `c ${ {d: 1}.d }` } e`",
		"a ? .5 : b?.c",
		"1 .toString(); 0x1f + 1e+5",
		"class A { #x = 1\n static y }",
		"a < !b; a-- > b",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		out, err := minifyJS(src)
		if err != nil {
			return
		}
		want, _ := lexJS(string(src))
		if got, err := lexJS(string(out)); err != nil || !sameJSTokens(got, want) {
			t.Fatalf("minified %q to %q, which lexes to different tokens (err %v)", src, out, err)
		}
		again, err := minifyJS(out)
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("minifying %q twice gave %q (err %v), want %q", src, again, err, out)
		}
	})
}

// FuzzMinifyCSS asserts the stylesheet minifier never panics and that its
// output is always accepted by a second pass.
func FuzzMinifyCSS(f *testing.F) {
	for _, s := range []string{
		"a { color: red; }",
		".a .b > .c, .d:not(.e) .f { width: calc(100% - 2rem) }",
		"@media screen and (max-width: 600px) { a { b: c } }",
		"a::before { content: \" { ; } /* */ \" } /* note */",
		".a\\: .b { background: url( x/*y*/z ) }",
		"a { --x: ; }",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		out, err := minifyCSS(src)
		if err != nil {
			return
		}
		if _, err := minifyCSS(out); err != nil {
			t.Fatalf("minified %q to %q, which a second pass rejects: %v", src, out, err)
		}
	})
}
