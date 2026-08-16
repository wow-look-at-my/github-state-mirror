package guards

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"

	"github.com/wow-look-at-my/go-containers/set"
)

// EVERY REQUEST THIS SERVICE SENDS IS ON THE DASHBOARD.
//
// A request nobody can see is a request nobody can account for. It spends
// rate-limit budget the operator is trying to explain, it can be slow, it can
// fail, and none of that reaches the chart they are staring at to answer
// "what is this thing doing right now?". The mirror's own sign-in flow was
// exactly that for a while: two real GitHub calls per dashboard login, on a
// client nothing observed, invisible on the very page they were serving.
//
// The property cannot be held by remembering. Instrumenting a call site
// covers the calls you thought of; the ones that go missing are the ones
// nobody thought of, including every call site added later. So the rule is
// about the CLIENT, not the call: an outbound HTTP client either reports what
// it sends, at its transport, or it is a hole.
//
// This check finds every construct in the repo that can originate an outbound
// request and requires each to be declared, with the mechanism that makes it
// visible written down next to it. A new one fails the build until someone
// says where it shows up. That is the whole point: the answer may well be
// "wrap it in httpobs" and take a minute, but it has to be an answer.
//
// Deliberately NOT a style rule. A bare http.Client is fine where something
// else observes it -- what is not fine is nobody having decided.

// OutboundKind names how a construct can reach the network.
type OutboundKind string

const (
	// KindClient is an http.Client composite literal.
	KindClient OutboundKind = "http.Client literal"
	// KindReverseProxy is an httputil.ReverseProxy literal, which sends
	// through its own Transport.
	KindReverseProxy OutboundKind = "httputil.ReverseProxy literal"
	// KindPackageLevel is one of net/http's package-level senders, which use
	// http.DefaultClient and can never be observed.
	KindPackageLevel OutboundKind = "net/http package-level request"
)

// packageLevelSenders are the net/http helpers that send on
// http.DefaultClient. There is no transport to wrap and no place to hang an
// observer, so one of these is always a hole -- the guard reports it and no
// allowlist entry should ever excuse it.
var packageLevelSenders = set.Of("Get", "Post", "PostForm", "Head")

// Outbound is one construct that can send a request.
type Outbound struct {
	File string
	Line int
	Kind OutboundKind
}

func (o Outbound) String() string {
	return fmt.Sprintf("%s:%d: %s", o.File, o.Line, o.Kind)
}

// FindOutbound reports every outbound-capable construct in one Go source file.
func FindOutbound(fset *token.FileSet, name string, src []byte) ([]Outbound, error) {
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, err
	}
	var out []Outbound
	add := func(pos token.Pos, kind OutboundKind) {
		p := fset.Position(pos)
		out = append(out, Outbound{File: name, Line: p.Line, Kind: kind})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			switch qualifiedName(node.Type) {
			case "http.Client":
				add(node.Pos(), KindClient)
			case "httputil.ReverseProxy":
				add(node.Pos(), KindReverseProxy)
			}
		case *ast.SelectorExpr:
			// http.DefaultClient used as a value, and the package-level
			// senders that use it implicitly.
			if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "http" {
				if node.Sel.Name == "DefaultClient" || packageLevelSenders.Contains(node.Sel.Name) {
					add(node.Pos(), KindPackageLevel)
				}
			}
		}
		return true
	})
	return out, nil
}

// qualifiedName renders a pkg.Type expression, following one level of pointer.
func qualifiedName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}
