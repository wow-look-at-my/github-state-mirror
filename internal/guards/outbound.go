package guards

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"

	"github.com/wow-look-at-my/go-containers/set"
)

// EVERY REQUEST THIS SERVICE SENDS IS ON THE DASHBOARD.

// OutboundKind names how a construct can reach the network.
type OutboundKind string

const (
	// KindClient is an http.Client composite literal.
	KindClient OutboundKind = "http.Client literal"
	// KindReverseProxy is an httputil.ReverseProxy literal, which sends
	KindReverseProxy OutboundKind = "httputil.ReverseProxy literal"
	// KindPackageLevel is of net/http's package-level senders, which use
	KindPackageLevel OutboundKind = "net/http package-level request"
)

// packageLevelSenders are the net/http helpers that send on
var packageLevelSenders = set.Of("Get", "Post", "PostForm", "Head")

// Outbound is construct that can send a request.
type Outbound struct {
	File string
	Line int
	Kind OutboundKind
}

func (o Outbound) String() string {
	return fmt.Sprintf("%s:%d: %s", o.File, o.Line, o.Kind)
}

// FindOutbound reports every outbound-capable construct in Go source file.
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

// qualifiedName renders a pkg.Type expression, following level of pointer.
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
