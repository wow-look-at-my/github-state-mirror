package guards

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// JSON TEXT IS PRODUCED BY MARSHALLING. FULL STOP.
//
// Every other way of building it is a guess about what the values contain. A
// concatenation escapes nothing at all -- `"sha":"` + sha + `"` is valid JSON
// only while sha holds no quote, backslash or control character, and
// `{"conclusion":` + fragment + `}` is worse, because a fragment carrying a
// quote or a backslash reshapes the document rather than one string. A format
// verb is no better: %s is raw, %d assumes the value is a number, and even %q
// is GO quoting, not JSON -- it emits \a, \v, \xNN and \UNNNNNNNN, none of
// which JSON accepts. Each of those is safe only by luck about today's inputs.
//
// So the rule has no exceptions to remember: if a string literal is JSON, it
// is a CONSTANT. The moment a runtime value belongs in the document, the
// document is built from a Go value and handed to encoding/json.
//
// This check reads a Go source file and reports both ways a value can reach a
// JSON literal:
//
//   - CONCATENATION: a + chain whose literal parts look like JSON, with any
//     non-literal operand.
//   - A FORMAT VERB: a JSON-looking format string (the literal argument of a
//     ...f-suffixed call) containing any verb.
//
// Restricting the format case to ...f-suffixed calls is what keeps a percent
// escape in an ordinary literal (a %20 in a URL) from reading as a verb.

// splicePlaceholder marks where a non-literal operand joins a + chain. It is a
// character no Go source literal can contain unescaped, so it cannot collide
// with real content.
const splicePlaceholder = '\x00'

// verbPattern matches a printf verb. The letter is required, so %20 in a URL
// is not mistaken for one.
var verbPattern = regexp.MustCompile(`%[-+# 0-9.*\[\]]*[a-zA-Z]`)

// Finding is one place JSON text is built from something other than a
// marshaller.
type Finding struct {
	File string
	Line int
	How  string // "concatenation" or "format verb"
	Text string // the offending fragment, for the failure message
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: JSON built by %s, not marshalled: %s", f.File, f.Line, f.How, f.Text)
}

// CheckFile parses Go source and reports every hand-built JSON document in it.
func CheckFile(fset *token.FileSet, name string, src []byte) ([]Finding, error) {
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	var out []Finding
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BinaryExpr:
			if node.Op != token.ADD {
				return true
			}
			// Only the outermost + of a chain: an inner one would report the
			// same document again.
			return !checkConcat(fset, node, &out)
		case *ast.CallExpr:
			checkFormatCall(fset, node, &out)
		}
		return true
	})
	return out, nil
}

// checkConcat reports a JSON document assembled with +, and returns whether
// the chain was a string concatenation it fully handled (so the caller stops
// descending into it).
func checkConcat(fset *token.FileSet, expr *ast.BinaryExpr, out *[]Finding) bool {
	parts := flattenAdd(expr)
	var b strings.Builder
	sawString, sawNonLiteral := false, false
	for _, p := range parts {
		if s, ok := stringLit(p); ok {
			sawString = true
			b.WriteString(s)
			continue
		}
		sawNonLiteral = true
		b.WriteByte(splicePlaceholder)
	}
	if !sawString {
		return false // numeric addition, or a chain of non-literals
	}
	if sawNonLiteral && looksLikeJSON(b.String()) {
		at := strings.IndexByte(b.String(), splicePlaceholder)
		*out = append(*out, Finding{
			File: fset.Position(expr.Pos()).Filename,
			Line: fset.Position(expr.Pos()).Line,
			How:  "concatenation",
			Text: excerpt(b.String(), at),
		})
	}
	return true
}

// checkFormatCall reports a JSON document assembled by a ...f-suffixed call
// (Sprintf, Fprintf, Errorf, ...). Any verb counts: %q is Go quoting rather
// than JSON quoting, and %s/%d place a value the encoder never sees.
func checkFormatCall(fset *token.FileSet, call *ast.CallExpr, out *[]Finding) {
	name := ""
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	if !strings.HasSuffix(name, "f") || len(call.Args) == 0 {
		return
	}
	format, pos, ok := formatArg(call.Args)
	if !ok || !looksLikeJSON(format) {
		return
	}
	if loc := verbPattern.FindStringIndex(format); loc != nil {
		*out = append(*out, Finding{
			File: fset.Position(pos).Filename,
			Line: fset.Position(pos).Line,
			How:  "format verb",
			Text: excerpt(format, loc[0]),
		})
	}
}

// formatArg finds the call's format string: the first argument that is a
// string literal (or a fold-able concatenation of them), since the format is
// preceded by a writer or a *testing.T in the Fprintf/Logf shapes.
func formatArg(args []ast.Expr) (string, token.Pos, bool) {
	for _, a := range args {
		if s, ok := foldStringLit(a); ok {
			return s, a.Pos(), true
		}
	}
	return "", token.NoPos, false
}

// foldStringLit evaluates a literal-only string expression, so a format
// written across several concatenated lines is scanned as the one string the
// compiler will build.
func foldStringLit(e ast.Expr) (string, bool) {
	if s, ok := stringLit(e); ok {
		return s, true
	}
	bin, ok := e.(*ast.BinaryExpr)
	if !ok || bin.Op != token.ADD {
		return "", false
	}
	var b strings.Builder
	for _, p := range flattenAdd(bin) {
		s, ok := stringLit(p)
		if !ok {
			return "", false
		}
		b.WriteString(s)
	}
	return b.String(), true
}

func flattenAdd(e ast.Expr) []ast.Expr {
	bin, ok := e.(*ast.BinaryExpr)
	if !ok || bin.Op != token.ADD {
		return []ast.Expr{e}
	}
	return append(flattenAdd(bin.X), flattenAdd(bin.Y)...)
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// looksLikeJSON reports whether a literal is a JSON document or fragment
// rather than prose that merely contains quotes. An object or array opening
// immediately followed by a string is decisive on its own; a bare key-value
// separator is not, because ordinary prose has them too -- `Get "x": %w` is an
// error message, and flagging it would teach the reader to skim this guard's
// output. So that weaker signal needs a brace or bracket somewhere as well.
func looksLikeJSON(s string) bool {
	if strings.Contains(s, `{"`) || strings.Contains(s, `["`) {
		return true
	}
	return strings.Contains(s, `":`) && strings.ContainsAny(s, "{}[]")
}

// excerpt quotes a window around the offending site so the failure names the
// exact spot rather than dumping a whole document.
func excerpt(s string, at int) string {
	const window = 28
	lo, hi := at-window, at+window
	if lo < 0 {
		lo = 0
	}
	if hi > len(s) {
		hi = len(s)
	}
	return strings.ReplaceAll(strconv.Quote(s[lo:hi]), "\x00", "<VALUE>")
}
