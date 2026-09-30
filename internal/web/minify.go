package web

import (
	"bytes"
	"fmt"
	"io/fs"
	"log"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Dependency-free, conservative CSS and JavaScript minifiers used to shrink
// the embedded dashboard assets once at startup.
//
// Both minifiers only drop comments and redundant whitespace. They never
// rename identifiers or rewrite tokens: the JavaScript minifier re-lexes its
// output and rejects it unless it carries the exact token stream of the
// input, and keeps a line break wherever automatic semicolon insertion could
// depend on it. Input the lexers cannot follow (an unterminated string,
// template, comment or regular expression) returns an error, and the caller
// serves the original bytes instead.

// minifiedStatic minifies every embedded stylesheet and first-party script
// once per process and returns the results keyed by embedded path. Vendored
// *.min.* files are served as embedded, as is any file whose minification
// fails or saves nothing.
var minifiedStatic = sync.OnceValue(func() map[string][]byte {
	assets := make(map[string][]byte)
	var before, after int
	_ = fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		minify := staticMinifier(path)
		if minify == nil {
			return nil
		}
		src, err := staticFS.ReadFile(path)
		if err != nil {
			return nil
		}
		out, err := minify(src)
		if err != nil {
			log.Printf("Warning: serving %s unminified: %v", strings.TrimPrefix(path, "static/"), err)
			return nil
		}
		if len(out) >= len(src) {
			return nil
		}
		assets[path] = out
		before += len(src)
		after += len(out)
		return nil
	})
	log.Printf("Minified %d static assets: %d KiB -> %d KiB", len(assets), before/1024, after/1024)
	return assets
})

// staticMinifier returns the minifier for an embedded path, or nil.
func staticMinifier(path string) func([]byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(path, ".min.js"), strings.HasSuffix(path, ".min.css"):
		return nil
	case strings.HasSuffix(path, ".js"):
		return minifyJS
	case strings.HasSuffix(path, ".css"):
		return minifyCSS
	}
	return nil
}

type jsTokenKind uint8

const (
	jsWord     jsTokenKind = iota // identifier, keyword or #private name
	jsNumber                      // numeric literal
	jsString                      // '...' or "..."
	jsTemplate                    // template literal, or one piece of it split at ${ }
	jsRegex                       // regular expression literal, flags included
	jsPunct                       // punctuator
)

type jsToken struct {
	kind jsTokenKind
	text string
	// spaceBefore reports whitespace or a comment between this token and the previous one.
	spaceBefore bool
	// lineBefore reports a line terminator, possibly inside a block comment,
	// between this token and the previous one.
	lineBefore bool
	// regexAfter marks a ")" closing an if/for/while/with head: a "/" after it
	// starts a regular expression, not a division.
	regexAfter bool
	// property marks a word that follows "." or "?.": a property name such as
	// the "of" in "a.of / 2", never a keyword.
	property bool
}

// jsPunctuators lists every punctuator, longest first, for longest-match lexing.
var jsPunctuators = []string{
	">>>=",
	"...", "===", "!==", "**=", "<<=", ">>=", ">>>", "&&=", "||=", "??=",
	"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "++", "--",
	"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "**", "<<", ">>",
	"{", "}", "(", ")", "[", "]", ";", ",", "<", ">", "+", "-", "*", "/",
	"%", "&", "|", "^", "!", "~", "?", ":", "=", ".", "@",
}

// jsKeywordsBeforeExpression are the words after which a "/" starts a
// regular expression rather than a division, unless the word is a property
// name (see jsToken.property).
var jsKeywordsBeforeExpression = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true,
	"new": true, "delete": true, "void": true, "throw": true, "case": true,
	"do": true, "else": true, "yield": true, "await": true, "extends": true,
	"default": true,
}

// jsNoLineBreakAfter are the words a restricted production forbids a line
// terminator after; a line break following them is always kept.
var jsNoLineBreakAfter = map[string]bool{
	"return": true, "break": true, "continue": true, "throw": true,
	"yield": true, "async": true,
}

// minifyJS strips comments and redundant whitespace from JavaScript source.
func minifyJS(src []byte) ([]byte, error) {
	toks, err := lexJS(string(src))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Grow(len(src))
	for i := range toks {
		t := &toks[i]
		if i > 0 {
			prev := &toks[i-1]
			switch {
			case t.lineBefore && !jsLineBreakRemovable(prev, t):
				out.WriteByte('\n')
			case t.spaceBefore && jsNeedsSpace(prev, t, out.Bytes()):
				out.WriteByte(' ')
			}
		}
		out.WriteString(t.text)
	}
	// The joins above are decided pairwise; confirm the whole output lexes
	// back to the same tokens before trusting it.
	check, err := lexJS(out.String())
	if err != nil || !sameJSTokens(check, toks) {
		return nil, fmt.Errorf("minified output does not preserve the token stream")
	}
	return out.Bytes(), nil
}

// sameJSTokens reports whether a and b hold the same token sequence,
// ignoring the whitespace around the tokens.
func sameJSTokens(a, b []jsToken) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].kind != b[i].kind || a[i].text != b[i].text {
			return false
		}
	}
	return true
}

// jsLineBreakRemovable reports whether dropping the line break between a and
// b leaves the parse unchanged. A line break only matters when automatic
// semicolon insertion would use it: before a token the grammar cannot accept
// in that position, or inside a restricted production. Neither can happen
// after a punctuator that still needs an operand, or before a token that can
// only continue the current statement.
func jsLineBreakRemovable(a, b *jsToken) bool {
	if a.kind == jsWord && jsNoLineBreakAfter[a.text] {
		return false
	}
	if b.kind == jsPunct && (b.text == "++" || b.text == "--") {
		return false // "a\n++b" is "a; ++b", not "a++ b"
	}
	switch a.kind {
	case jsPunct:
		switch a.text {
		case ")", "]", "}", "++", "--":
		default:
			return true
		}
	case jsTemplate:
		if strings.HasSuffix(a.text, "${") {
			return true
		}
	}
	switch b.kind {
	case jsPunct:
		switch b.text {
		case "}", ")", "]", ",", ";", ".", "?.":
			return true
		}
	case jsTemplate:
		return b.text[0] == '}'
	}
	return false
}

// jsNeedsSpace reports whether a and b would lex differently if b were
// appended to out, which ends with a, without a separator.
func jsNeedsSpace(a, b *jsToken, out []byte) bool {
	last, first := a.text[len(a.text)-1], b.text[0]
	switch a.kind {
	case jsWord, jsNumber, jsRegex:
		if jsIdentByte(first) || first == '\\' || first == '#' || first >= utf8.RuneSelf {
			return true
		}
		if a.kind == jsNumber && first == '.' {
			return true
		}
	case jsPunct:
		// A punctuator can merge across several adjacent ones: ".." and "."
		// become "...". The longest punctuator has four bytes, so any merge
		// starts within the last three bytes written.
		for k := 1; k <= 3 && k <= len(out); k++ {
			if len(jsPunctAt(string(out[len(out)-k:])+b.text, 0)) > k {
				return true
			}
		}
	}
	switch {
	case last == '.' && jsDigit(first): // "a?. 5" would become "a ? .5"
		return true
	case last == '/' && (first == '/' || first == '*'): // would open a comment
		return true
	case last == '<' && first == '!': // "<!--" is a comment in classic scripts
		return true
	case last == '-' && first == '>': // so is "-->" at the start of a line
		return true
	}
	return false
}

// lexJS splits JavaScript source into tokens, dropping whitespace and comments
// but recording where they were.
func lexJS(src string) ([]jsToken, error) {
	var (
		toks []jsToken
		// braces holds, for each open template substitution, how many "{"
		// are open inside it; the "}" that finds zero resumes the template.
		braces []int
		// parens holds, for each open "(", whether it opened an
		// if/for/while/with head.
		parens      []bool
		space, line bool
	)
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n' || c == '\r':
			space, line = true, true
			i++
			continue
		case c == ' ' || c == '\t' || c == '\v' || c == '\f':
			space = true
			i++
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			space = true
			i = jsLineEnd(src, i+2)
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment at offset %d", i)
			}
			if jsLineEnd(src[i+2:i+2+end], 0) < end {
				line = true
			}
			space = true
			i += 2 + end + 2
			continue
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(src[i:])
			if r == '\u2028' || r == '\u2029' {
				space, line = true, true
				i += size
				continue
			}
			if unicode.IsSpace(r) || r == '\ufeff' {
				space = true
				i += size
				continue
			}
		}

		var prev *jsToken
		if len(toks) > 0 {
			prev = &toks[len(toks)-1]
		}
		tok := jsToken{spaceBefore: space, lineBefore: line}
		space, line = false, false
		start := i

		switch {
		case c == '\'' || c == '"':
			end, err := jsScanString(src, i)
			if err != nil {
				return nil, err
			}
			tok.kind, i = jsString, end
		case c == '`' || (c == '}' && len(braces) > 0 && braces[len(braces)-1] == 0):
			if c == '}' {
				braces = braces[:len(braces)-1]
			}
			end, subst, err := jsScanTemplate(src, i+1)
			if err != nil {
				return nil, fmt.Errorf("unterminated template at offset %d", start)
			}
			if subst {
				braces = append(braces, 0)
			}
			tok.kind, i = jsTemplate, end
		case jsDigit(c) || (c == '.' && i+1 < len(src) && jsDigit(src[i+1])):
			tok.kind, i = jsNumber, jsScanNumber(src, i)
		case jsIdentByte(c) || c == '\\' || c == '#' || c >= utf8.RuneSelf:
			tok.kind, i = jsWord, jsScanWord(src, i)
			tok.property = prev != nil && prev.kind == jsPunct && (prev.text == "." || prev.text == "?.")
		case c == '/' && jsRegexAllowed(prev):
			end, err := jsScanRegex(src, i)
			if err != nil {
				return nil, err
			}
			tok.kind, i = jsRegex, end
		default:
			p := jsPunctAt(src, i)
			if p == "" {
				return nil, fmt.Errorf("unexpected character %q at offset %d", c, i)
			}
			tok.kind, i = jsPunct, i+len(p)
			switch p {
			case "{":
				if len(braces) > 0 {
					braces[len(braces)-1]++
				}
			case "}":
				if len(braces) > 0 {
					braces[len(braces)-1]--
				}
			case "(":
				parens = append(parens, jsOpensHead(toks))
			case ")":
				if len(parens) > 0 {
					tok.regexAfter = parens[len(parens)-1]
					parens = parens[:len(parens)-1]
				}
			}
		}
		tok.text = src[start:i]
		toks = append(toks, tok)
	}
	if len(braces) > 0 {
		return nil, fmt.Errorf("unterminated template substitution")
	}
	return toks, nil
}

// jsRegexAllowed reports whether a "/" following prev starts a regular
// expression. It is a heuristic: a "}" is taken to end a block, not an
// object literal.
func jsRegexAllowed(prev *jsToken) bool {
	if prev == nil {
		return true
	}
	switch prev.kind {
	case jsNumber, jsString, jsRegex:
		return false
	case jsTemplate:
		return strings.HasSuffix(prev.text, "${")
	case jsWord:
		return !prev.property && jsKeywordsBeforeExpression[prev.text]
	}
	switch prev.text {
	case ")":
		return prev.regexAfter
	case "]", "++", "--":
		return false
	}
	return true
}

// jsOpensHead reports whether a "(" following toks opens an if, for, while
// or with head, including "for await (".
func jsOpensHead(toks []jsToken) bool {
	keyword := func(back int) string {
		if back > len(toks) {
			return ""
		}
		if tok := toks[len(toks)-back]; tok.kind == jsWord && !tok.property {
			return tok.text
		}
		return ""
	}
	switch keyword(1) {
	case "if", "for", "while", "with":
		return true
	case "await":
		return keyword(2) == "for"
	}
	return false
}

// jsPunctAt returns the longest punctuator at src[i:], or "".
func jsPunctAt(src string, i int) string {
	for _, p := range jsPunctuators {
		if strings.HasPrefix(src[i:], p) {
			// "?." followed by a digit is "?" and a number: a ? .5 : b
			if p == "?." && i+2 < len(src) && jsDigit(src[i+2]) {
				continue
			}
			return p
		}
	}
	return ""
}

// jsLineEnd returns the index of the first line terminator at or after i,
// or len(src).
func jsLineEnd(src string, i int) int {
	for i < len(src) {
		switch c := src[i]; {
		case c == '\n' || c == '\r':
			return i
		case c == 0xe2 && (strings.HasPrefix(src[i:], "\u2028") || strings.HasPrefix(src[i:], "\u2029")):
			return i
		}
		i++
	}
	return len(src)
}

func jsScanString(src string, i int) (int, error) {
	quote := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++ // skip the escaped character; a CRLF continuation spans two bytes
			if j+1 < len(src) && src[j] == '\r' && src[j+1] == '\n' {
				j++
			}
		case quote:
			return j + 1, nil
		case '\n', '\r':
			return 0, fmt.Errorf("unterminated string at offset %d", i)
		}
	}
	return 0, fmt.Errorf("unterminated string at offset %d", i)
}

// jsScanTemplate scans template characters from i, just after the opening
// "`" or the "}" closing a substitution. It returns the index after the
// closing "`" or "${", and whether a substitution opened.
func jsScanTemplate(src string, i int) (int, bool, error) {
	for i < len(src) {
		switch src[i] {
		case '\\':
			i += 2
		case '`':
			return i + 1, false, nil
		case '$':
			if i+1 < len(src) && src[i+1] == '{' {
				return i + 2, true, nil
			}
			i++
		default:
			i++
		}
	}
	return 0, false, fmt.Errorf("unterminated template")
}

func jsScanRegex(src string, i int) (int, error) {
	inClass := false
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; {
		case c == '\\':
			j++
			if j < len(src) && (src[j] == '\n' || src[j] == '\r') {
				return 0, fmt.Errorf("unterminated regular expression at offset %d", i)
			}
		case c == '\n' || c == '\r':
			return 0, fmt.Errorf("unterminated regular expression at offset %d", i)
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
		case c == '/':
			j++
			for j < len(src) && jsIdentByte(src[j]) {
				j++
			}
			return j, nil
		}
	}
	return 0, fmt.Errorf("unterminated regular expression at offset %d", i)
}

func jsScanNumber(src string, i int) int {
	j := i
	if src[j] == '0' && j+1 < len(src) && strings.IndexByte("xXoObB", src[j+1]) >= 0 {
		j += 2
		for j < len(src) && (jsHexDigit(src[j]) || src[j] == '_' || src[j] == 'n') {
			j++
		}
		return j
	}
	digits := func() {
		for j < len(src) && (jsDigit(src[j]) || src[j] == '_') {
			j++
		}
	}
	digits()
	if j < len(src) && src[j] == '.' {
		j++
		digits()
	}
	if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < len(src) && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k < len(src) && jsDigit(src[k]) {
			j = k
			digits()
		}
	}
	if j < len(src) && src[j] == 'n' {
		j++
	}
	return j
}

func jsScanWord(src string, i int) int {
	j := i
	if src[j] == '#' {
		j++
	}
	for j < len(src) {
		c := src[j]
		switch {
		case jsIdentByte(c):
			j++
		case c == '\\': // \uXXXX or \u{X...} escape
			j++
			if strings.HasPrefix(src[j:], "u{") {
				if end := strings.IndexByte(src[j:], '}'); end >= 0 {
					j += end + 1
				}
			}
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(src[j:])
			if unicode.IsSpace(r) || r == '\ufeff' {
				return j
			}
			j += size
		default:
			return j
		}
	}
	return j
}

func jsDigit(c byte) bool { return c >= '0' && c <= '9' }

func jsHexDigit(c byte) bool {
	return jsDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// jsIdentByte reports whether c is an ASCII identifier-part character.
func jsIdentByte(c byte) bool {
	return c == '$' || c == '_' || jsDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// minifyCSS strips comments and redundant whitespace from a stylesheet. It
// keeps every space a selector or value might depend on: descendant
// combinators, the operators of calc(), and the space before "(" that keeps
// "and (" in a media query from becoming a function call.
func minifyCSS(src []byte) ([]byte, error) {
	s := string(src)
	var out bytes.Buffer
	out.Grow(len(s))
	space := false
	// structural is the last emitted byte when it was a bare delimiter, or 0
	// after a string, escape or url( argument, whose bytes never merge.
	var structural byte
	// semicolon is the output offset of a trailing structural ";", or -1.
	semicolon := -1
	emit := func(text string, literal bool) {
		first := text[0]
		if space && out.Len() > 0 {
			switch {
			case structural == ':' && (first == ';' || first == '}'):
				out.WriteByte(' ') // "--x: ;" is an empty custom property; "--x:;" is invalid in older engines
			case !cssNoSpaceAfter(structural) && (literal || !cssNoSpaceBefore(first)):
				out.WriteByte(' ')
			}
		}
		space = false
		if !literal && first == '}' && semicolon >= 0 && semicolon == out.Len()-1 {
			out.Truncate(semicolon)
		}
		semicolon = -1
		if !literal && text == ";" {
			semicolon = out.Len()
		}
		out.WriteString(text)
		structural = 0
		if !literal && len(text) == 1 {
			structural = text[0]
		}
	}

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case strings.IndexByte(cssWhitespace, c) >= 0:
			space = true
			i++
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment at offset %d", i)
			}
			i += 2 + end + 2
			// A comment separates tokens without being whitespace. Next to
			// whitespace it collapses with it. Between two tokens it can only
			// go where they cannot merge: dropped, "1px/**/2px" would become
			// one token, and as a space ".a/**/.b" would become a descendant
			// selector. Elsewhere an empty comment keeps them apart.
			switch {
			case space || out.Len() == 0 || i >= len(s) || strings.IndexByte(cssWhitespace, s[i]) >= 0:
				space = true
			case !cssCommentDroppable(out.Bytes()[out.Len()-1], s[i]):
				emit("/**/", true)
			}
		case c == '"' || c == '\'':
			end, err := cssScanString(s, i)
			if err != nil {
				return nil, err
			}
			emit(s[i:end], true)
			i = end
		case c == '\\':
			text, end := cssEscape(s, i)
			emit(text, true)
			i = end
		case (c == 'u' || c == 'U') && cssURLStart(s, i, out.Bytes()):
			end, ok := cssScanUnquotedURL(s, i+4)
			if !ok {
				emit(s[i:i+4], false)
				i += 4
				continue
			}
			emit(s[i:i+4]+strings.Trim(s[i+4:end-1], cssWhitespace)+")", true)
			i = end
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			emit(s[i:i+size], false)
			i += size
		}
	}
	return out.Bytes(), nil
}

// cssWhitespace lists the CSS whitespace bytes; unlike Go's, it excludes "\v".
const cssWhitespace = " \t\n\r\f"

func cssNoSpaceAfter(c byte) bool { return c != 0 && strings.IndexByte("{};,>~(:", c) >= 0 }

func cssNoSpaceBefore(c byte) bool { return strings.IndexByte("{};,>~)!", c) >= 0 }

// cssCommentDroppable reports whether a comment between the output byte
// before and the source byte after it can be removed without the two
// merging into another token: one side is a delimiter that is a token of its
// own. "(" only qualifies before the comment, since "f/**/(" would become
// the function token "f(".
func cssCommentDroppable(before, after byte) bool {
	return strings.IndexByte("{}();,:[]>", before) >= 0 || strings.IndexByte("{});,:[]>~!", after) >= 0
}

// cssEscape returns the escape starting at s[i] == '\\' and the index after
// it. A hex escape is up to six hex digits and one whitespace character
// (CRLF counts as one) ending it. That whitespace is part of the escape, so
// it stays with the escape, as a space, instead of collapsing with any
// whitespace after it: `.a\31  .b` is class "a1" and then a descendant.
func cssEscape(s string, i int) (string, int) {
	j := i + 1
	if j >= len(s) {
		return s[i:j], j
	}
	if !jsHexDigit(s[j]) {
		_, size := utf8.DecodeRuneInString(s[j:])
		return s[i : j+size], j + size
	}
	for digits := 0; digits < 6 && j < len(s) && jsHexDigit(s[j]); digits++ {
		j++
	}
	switch {
	case strings.HasPrefix(s[j:], "\r\n"):
		return s[i:j] + " ", j + 2
	case j < len(s) && strings.IndexByte(cssWhitespace, s[j]) >= 0:
		return s[i:j] + " ", j + 1
	}
	return s[i:j], j
}

func cssScanString(s string, i int) (int, error) {
	quote := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case quote:
			return j + 1, nil
		case '\n', '\r', '\f':
			return 0, fmt.Errorf("unterminated string at offset %d", i)
		}
	}
	return 0, fmt.Errorf("unterminated string at offset %d", i)
}

// cssURLStart reports whether s[i:] opens a url( function: the name is not
// the tail of a longer identifier.
func cssURLStart(s string, i int, out []byte) bool {
	if len(s)-i < 4 || !strings.EqualFold(s[i:i+4], "url(") {
		return false
	}
	if len(out) == 0 {
		return true
	}
	last := out[len(out)-1]
	return !jsIdentByte(last) && last != '-' && last != '\\' && last < utf8.RuneSelf
}

// cssScanUnquotedURL scans an unquoted url( argument, whose contents may hold
// "/*" or other characters that are not comments or tokens, from i (just past
// "url("). It returns the index after ")", or false for a quoted argument or
// one holding escapes, quotes or "(", which the generic path handles.
func cssScanUnquotedURL(s string, i int) (int, bool) {
	j := i
	for j < len(s) && strings.IndexByte(cssWhitespace, s[j]) >= 0 {
		j++
	}
	for ; j < len(s); j++ {
		switch s[j] {
		case ')':
			return j + 1, true
		case '"', '\'', '(', '\\':
			return 0, false
		}
	}
	return 0, false
}
