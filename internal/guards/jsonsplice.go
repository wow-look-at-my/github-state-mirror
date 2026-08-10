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

// THE JSON-SPLICE CHECK.
//
// A value dropped between a JSON string literal's own quotes is escaped by
// nothing. `"sha":"` + sha + `"` is valid JSON only because today's value
// happens to contain no quote, backslash, newline or control character, and
// the day it does the document silently becomes something else -- the same
// defect as an SQL string built with +, and the same one the jq rule
// (--arg, never shell interpolation) exists to prevent.
//
// The rule this enforces: a runtime value may not sit inside a JSON string's
// quotes. Put it there with a verb that supplies and escapes its own quotes
// (%q), give it its own non-string slot (%d for a number, %s for a raw
// null/true/nested fragment), or marshal the document from a Go value.
//
// The check reads a Go source file and reports each place where a value lands
// inside JSON string quotes, by either route:
//
//   - CONCATENATION: a + chain whose literal parts look like JSON, with a
//     non-literal operand spliced in while the scan is inside a string.
//   - A FORMAT VERB: a format string (the first literal argument of a
//     ...f-suffixed call) that looks like JSON, with a verb inside a string.
//
// Both routes run through one scanner, which is why %q needs no special case:
// %q sits OUTSIDE the quotes it produces, so the scan is not in a string when
// it reaches it.

// splicePlaceholder marks where a non-literal operand joins a + chain. It is a
// character no Go source literal can contain unescaped, so it cannot collide
// with real content.
const splicePlaceholder = '\x00'

// verbPattern matches a printf verb. The letter is required, so a percent
// escape in a URL (%20) is not mistaken for one.
var verbPattern = regexp.MustCompile(`%[-+# 0-9.*\[\]]*[a-zA-Z]`)

// Finding is one place a value lands inside JSON string quotes.
type Finding struct {
	File string
	Line int
	How  string // "concatenation" or "format verb"
	Text string // the offending fragment, for the failure message
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: value spliced into JSON string quotes by %s: %s", f.File, f.Line, f.How, f.Text)
}

// CheckFile parses Go source and reports every JSON splice in it.
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
			// same splice again.
			return !checkConcat(fset, node, &out)
		case *ast.CallExpr:
			checkFormatCall(fset, node, &out)
		}
		return true
	})
	return out, nil
}

// checkConcat reports splices in a + chain and returns whether the chain is a
// string concatenation it fully handled (so the caller stops descending).
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
		for _, at := range insideStringQuotes(b.String()) {
			if at.isPlaceholder {
				*out = append(*out, Finding{
					File: fset.Position(expr.Pos()).Filename,
					Line: fset.Position(expr.Pos()).Line,
					How:  "concatenation",
					Text: excerpt(b.String(), at.offset),
				})
			}
		}
	}
	return true
}

// checkFormatCall reports verbs sitting inside JSON string quotes in the
// format argument of a ...f-suffixed call (Sprintf, Fprintf, Errorf, ...).
// Restricting it to those calls is what keeps a stray percent in an ordinary
// literal from reading as a verb.
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
	for _, at := range insideStringQuotes(format) {
		if at.isVerb {
			*out = append(*out, Finding{
				File: fset.Position(pos).Filename,
				Line: fset.Position(pos).Line,
				How:  "format verb",
				Text: excerpt(format, at.offset),
			})
		}
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
// rather than prose that merely contains quotes. A key-value separator or an
// object/array opening immediately followed by a key is the signature.
func looksLikeJSON(s string) bool {
	return strings.Contains(s, `":`) || strings.Contains(s, `{"`) || strings.Contains(s, `["`)
}

type siteAt struct {
	offset        int
	isPlaceholder bool
	isVerb        bool
}

// insideStringQuotes walks a JSON template and reports every placeholder and
// every printf verb that occurs while the scan is INSIDE a JSON string. It
// tracks the string state itself rather than pattern-matching around the
// interesting characters, which is what makes `"k": %q` (outside, fine) and
// `"k": "v/%s"` (inside, not fine) come out different.
func insideStringQuotes(s string) []siteAt {
	var out []siteAt
	inString := false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case inString && c == '\\':
			i += 2 // an escape, whatever it escapes, never ends the string
			continue
		case c == '"':
			inString = !inString
		case c == splicePlaceholder:
			if inString {
				out = append(out, siteAt{offset: i, isPlaceholder: true})
			}
			// A spliced value can itself open or close a quote, so the state
			// after it is unknowable. Assume it is balanced: the reported
			// finding is the point either way.
		case c == '%':
			if loc := verbPattern.FindStringIndex(s[i:]); loc != nil && loc[0] == 0 {
				if inString {
					out = append(out, siteAt{offset: i, isVerb: true})
				}
				i += loc[1]
				continue
			}
		}
		i++
	}
	return out
}

// excerpt quotes a window around the offending site so the failure names the
// exact spot rather than dumping a whole fixture body.
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
