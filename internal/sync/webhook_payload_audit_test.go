package sync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE PAYLOAD AUDIT: a build gate over every dispatcher event type.
// see docs/webhooks/payload-audit-test.md

// Exception docs are read at run time; touch a .go file or delete build/server
// to force a re-run locally after a doc-only edit (go-toolchain's fingerprint skip watches only .go files).
const payloadUnusedDir = "../../docs/webhooks/payload-unused"

// What reading the delivery body looks like: the typed parsers in
// internal/webhook, or touching the raw bytes directly.
func readsDelivery(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		// webhook.Parse*(...) — the typed payload parsers.
		if x.Name == "webhook" && strings.HasPrefix(sel.Sel.Name, "Parse") {
			found = true
			return false
		}
		// event.Raw — the delivery body itself.
		if x.Name == "event" && sel.Sel.Name == "Raw" {
			found = true
			return false
		}
		return true
	})
	return found
}

// Names of functions called from n, both plain calls and method calls on the
// dispatcher receiver, so the walk can follow delegation.
func calleeNames(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			out = append(out, fn.Name)
		case *ast.SelectorExpr:
			out = append(out, fn.Sel.Name)
		}
		return true
	})
	return out
}

// TestWebhookHandlersConsumeTheirPayload is the audit. It reads this package's
// own source, so it cannot drift from the code it describes.
func TestWebhookHandlersConsumeTheirPayload(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)
	pkg, ok := pkgs["sync"]
	require.True(t, ok, "the sync package must parse")

	// Every function in the package, by name, so delegation can be followed.
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	require.NotEmpty(t, funcs)

	// Does fn read the delivery, directly or through anything it calls?
	var consumes func(name string, seen map[string]bool) bool
	consumes = func(name string, seen map[string]bool) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		fd, ok := funcs[name]
		if !ok {
			return false // outside this package; the parsers themselves match above
		}
		if readsDelivery(fd.Body) {
			return true
		}
		for _, callee := range calleeNames(fd.Body) {
			// Never credit the generic envelope absorb (see the scoping rules).
			if callee == "absorbRepoFromPayload" {
				continue
			}
			if consumes(callee, seen) {
				return true
			}
		}
		return false
	}

	// The dispatcher's switch IS the list of events we use. Read it from the
	// AST so a newly handled event is audited the moment it is added — nobody
	// has to remember to update a list here.
	handle, ok := funcs["handle"]
	require.True(t, ok, "WebhookDispatcher.handle must exist")

	handlerFor := map[string]string{}
	ast.Inspect(handle.Body, func(node ast.Node) bool {
		cc, ok := node.(*ast.CaseClause)
		if !ok || len(cc.List) == 0 {
			return true // the default arm has no List: out of scope by design
		}
		var events []string
		for _, e := range cc.List {
			lit, ok := e.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			events = append(events, v)
		}
		// The handler this arm dispatches to.
		var handler string
		for _, name := range calleeNames(cc) {
			if strings.HasPrefix(name, "on") {
				handler = name
				break
			}
		}
		if handler == "" {
			return true
		}
		for _, e := range events {
			handlerFor[e] = handler
		}
		return true
	})
	require.NotEmpty(t, handlerFor, "the audit must find the dispatcher's event switch")

	events := make([]string, 0, len(handlerFor))
	for e := range handlerFor {
		events = append(events, e)
	}
	sort.Strings(events)

	consuming, exempt := 0, 0
	for _, event := range events {
		handler := handlerFor[event]
		uses := consumes(handler, map[string]bool{})
		docPath := filepath.Join(payloadUnusedDir, event+".md")
		doc, docErr := os.ReadFile(docPath)
		hasDoc := docErr == nil

		switch {
		case uses && hasDoc:
			assert.Fail(t, "stale payload-unused exception",
				"%q: %s DOES read the delivery, but %s still exists. An event whose handler uses its payload has no "+
					"exception — delete the doc in the change that started consuming it.", event, handler, docPath)
		case uses:
			consuming++
		case hasDoc:
			assertExceptionDoc(t, event, docPath, string(doc))
			exempt++
		default:
			assert.Fail(t, "webhook payload discarded",
				"%q is handled by %s, which never reads the delivery body. GitHub already sent us this state; answering "+
					"it with an invalidation (or nothing) buys the same facts back over HTTP. Either apply the payload, "+
					"or add %s explaining in detail why this event's payload cannot be used.", event, handler, docPath)
		}
	}
	t.Logf("payload audit: %d handled event type(s) consume their delivery, %d carry a documented exception",
		consuming, exempt)
}

// An exception must be an explanation, not a silencer. These are the questions
// a reader needs answered; an author who cannot answer them does not have an
// exception, they have an event whose payload should be getting used.
func assertExceptionDoc(t *testing.T, event, docPath, doc string) {
	t.Helper()
	for _, heading := range []string{
		"## What the payload carries",
		"## Why it is not applied",
		"## What we do instead, and what that costs",
		"## What would have to change",
	} {
		assert.Contains(t, doc, heading, "%s must answer %q", docPath, heading)
	}
	assert.GreaterOrEqual(t, len(doc), 1500,
		"%s is %d chars: an exception must be explained in detail. If there is not that much to say, the handler "+
			"probably should be applying the payload.", docPath, len(doc))
	assert.Contains(t, doc, event, "%s never names the event (%s) it exempts", docPath, event)
}

// audit cache probe
