package api

import (
	"errors"
	"strings"
)

// A small GraphQL reader for the cached query shapes. It reads the executable subset of the GraphQL grammar.

var errGQLSyntax = errors.New("graphql: unsupported syntax")

type gqlToken struct {
	kind byte // 'n' name, 'p' punctuator, 's' string, '#' number
	text string
}

// gqlTokenize splits a document into tokens. Whitespace, commas and comments
// are not tokens in GraphQL, so spellings of one query give one stream.
func gqlTokenize(src string) ([]gqlToken, error) {
	var out []gqlToken
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' && src[i] != '\r' {
				i++
			}
		case strings.HasPrefix(src[i:], "..."):
			out = append(out, gqlToken{'p', "..."})
			i += 3
		case strings.IndexByte("!$():=[]{}|&", c) >= 0:
			out = append(out, gqlToken{'p', string(c)})
			i++
		case c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z'):
			j := i + 1
			for j < len(src) && (src[j] == '_' || (src[j]|0x20 >= 'a' && src[j]|0x20 <= 'z') || (src[j] >= '0' && src[j] <= '9')) {
				j++
			}
			out = append(out, gqlToken{'n', src[i:j]})
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(src) && strings.IndexByte("0123456789.eE+-", src[j]) >= 0 {
				j++
			}
			out = append(out, gqlToken{'#', src[i:j]})
			i = j
		case c == '"':
			if strings.HasPrefix(src[i:], `"""`) {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, errGQLSyntax
				}
				out = append(out, gqlToken{'s', src[i : i+3+end+3]})
				i += 3 + end + 3
				continue
			}
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				if j < len(src) && (src[j] == '\n' || src[j] == '\r') {
					return nil, errGQLSyntax
				}
				j++
			}
			if j >= len(src) {
				return nil, errGQLSyntax
			}
			out = append(out, gqlToken{'s', src[i : j+1]})
			i = j + 1
		case c == 0xEF && strings.HasPrefix(src[i:], "\xEF\xBB\xBF"):
			i += 3
		default:
			return nil, errGQLSyntax
		}
	}
	return out, nil
}

// gqlValue is an argument value.
type gqlValue struct {
	Var    string
	Raw    string
	Str    string
	IsStr  bool
	List   []gqlValue
	Object map[string]gqlValue
}

type gqlSelection struct {
	Field     string // empty for a fragment spread
	Alias     string
	Args      map[string]gqlValue
	Selection []gqlSelection
	Spread    string // a named fragment spread
}

type gqlOperation struct {
	Kind      string // "query", "mutation", "subscription"
	Name      string
	Selection []gqlSelection
}

type gqlFragment struct {
	On        string
	Selection []gqlSelection
}

type gqlDocument struct {
	Operations []gqlOperation
	Fragments  map[string]gqlFragment
	// Canonical is the token stream joined by single spaces.
	Canonical string
}

type gqlParser struct {
	toks []gqlToken
	pos  int
}

func (p *gqlParser) peek() gqlToken {
	if p.pos >= len(p.toks) {
		return gqlToken{}
	}
	return p.toks[p.pos]
}

func (p *gqlParser) next() gqlToken {
	t := p.peek()
	p.pos++
	return t
}

func (p *gqlParser) isPunct(s string) bool {
	t := p.peek()
	return t.kind == 'p' && t.text == s
}

func (p *gqlParser) expectPunct(s string) error {
	if !p.isPunct(s) {
		return errGQLSyntax
	}
	p.pos++
	return nil
}

func (p *gqlParser) name() (string, error) {
	t := p.next()
	if t.kind != 'n' {
		return "", errGQLSyntax
	}
	return t.text, nil
}

// parseGraphQL reads a document in the supported subset.
func parseGraphQL(src string) (gqlDocument, error) {
	toks, err := gqlTokenize(src)
	if err != nil {
		return gqlDocument{}, err
	}
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.text
	}
	doc := gqlDocument{Fragments: map[string]gqlFragment{}, Canonical: strings.Join(parts, " ")}
	p := &gqlParser{toks: toks}
	for p.pos < len(p.toks) {
		t := p.peek()
		switch {
		case t.kind == 'p' && t.text == "{":
			sel, err := p.selectionSet()
			if err != nil {
				return gqlDocument{}, err
			}
			doc.Operations = append(doc.Operations, gqlOperation{Kind: "query", Selection: sel})
		case t.kind == 'n' && t.text == "fragment":
			p.pos++
			name, err := p.name()
			if err != nil {
				return gqlDocument{}, err
			}
			if on, err := p.name(); err != nil || on != "on" {
				return gqlDocument{}, errGQLSyntax
			}
			typ, err := p.name()
			if err != nil {
				return gqlDocument{}, err
			}
			sel, err := p.selectionSet()
			if err != nil {
				return gqlDocument{}, err
			}
			if _, dup := doc.Fragments[name]; dup {
				return gqlDocument{}, errGQLSyntax
			}
			doc.Fragments[name] = gqlFragment{On: typ, Selection: sel}
		case t.kind == 'n' && (t.text == "query" || t.text == "mutation" || t.text == "subscription"):
			p.pos++
			op := gqlOperation{Kind: t.text}
			if p.peek().kind == 'n' {
				op.Name = p.next().text
			}
			if p.isPunct("(") {
				if err := p.skipVariableDefinitions(); err != nil {
					return gqlDocument{}, err
				}
			}
			sel, err := p.selectionSet()
			if err != nil {
				return gqlDocument{}, err
			}
			op.Selection = sel
			doc.Operations = append(doc.Operations, op)
		default:
			return gqlDocument{}, errGQLSyntax
		}
	}
	return doc, nil
}

// skipVariableDefinitions reads "($a: T = default, ...)". GitHub checks the
// types; the cache key already holds the whole token stream.
func (p *gqlParser) skipVariableDefinitions() error {
	if err := p.expectPunct("("); err != nil {
		return err
	}
	for !p.isPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return err
		}
		if _, err := p.name(); err != nil {
			return err
		}
		if err := p.expectPunct(":"); err != nil {
			return err
		}
		if err := p.skipType(); err != nil {
			return err
		}
		if p.isPunct("=") {
			p.pos++
			if _, err := p.value(); err != nil {
				return err
			}
		}
	}
	p.pos++
	return nil
}

func (p *gqlParser) skipType() error {
	if p.isPunct("[") {
		p.pos++
		if err := p.skipType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else if _, err := p.name(); err != nil {
		return err
	}
	if p.isPunct("!") {
		p.pos++
	}
	return nil
}

func (p *gqlParser) selectionSet() ([]gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []gqlSelection
	for !p.isPunct("}") {
		if p.pos >= len(p.toks) {
			return nil, errGQLSyntax
		}
		if p.isPunct("...") {
			p.pos++
			name, err := p.name()
			// An inline fragment ("... on T") is outside the subset.
			if err != nil || name == "on" {
				return nil, errGQLSyntax
			}
			out = append(out, gqlSelection{Spread: name})
			continue
		}
		name, err := p.name()
		if err != nil {
			return nil, err
		}
		sel := gqlSelection{Field: name}
		if p.isPunct(":") {
			p.pos++
			sel.Alias = name
			if sel.Field, err = p.name(); err != nil {
				return nil, err
			}
		}
		if p.isPunct("(") {
			p.pos++
			sel.Args = map[string]gqlValue{}
			for !p.isPunct(")") {
				arg, err := p.name()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.value()
				if err != nil {
					return nil, err
				}
				if _, dup := sel.Args[arg]; dup {
					return nil, errGQLSyntax
				}
				sel.Args[arg] = v
			}
			p.pos++
		}
		if p.isPunct("{") {
			if sel.Selection, err = p.selectionSet(); err != nil {
				return nil, err
			}
		}
		out = append(out, sel)
	}
	p.pos++
	if len(out) == 0 {
		return nil, errGQLSyntax
	}
	return out, nil
}

func (p *gqlParser) value() (gqlValue, error) {
	t := p.next()
	switch {
	case t.kind == 'p' && t.text == "$":
		name, err := p.name()
		return gqlValue{Var: name}, err
	case t.kind == 'p' && t.text == "[":
		v := gqlValue{Raw: "[", List: []gqlValue{}}
		for !p.isPunct("]") {
			if p.pos >= len(p.toks) {
				return gqlValue{}, errGQLSyntax
			}
			item, err := p.value()
			if err != nil {
				return gqlValue{}, err
			}
			v.List = append(v.List, item)
		}
		p.pos++
		return v, nil
	case t.kind == 'p' && t.text == "{":
		v := gqlValue{Raw: "{", Object: map[string]gqlValue{}}
		for !p.isPunct("}") {
			key, err := p.name()
			if err != nil {
				return gqlValue{}, err
			}
			if err := p.expectPunct(":"); err != nil {
				return gqlValue{}, err
			}
			item, err := p.value()
			if err != nil {
				return gqlValue{}, err
			}
			v.Object[key] = item
		}
		p.pos++
		return v, nil
	case t.kind == 's':
		if strings.HasPrefix(t.text, `"""`) {
			return gqlValue{Raw: t.text, Str: t.text[3 : len(t.text)-3], IsStr: true}, nil
		}
		s, ok := gqlUnquote(t.text)
		if !ok {
			return gqlValue{}, errGQLSyntax
		}
		return gqlValue{Raw: t.text, Str: s, IsStr: true}, nil
	case t.kind == 'n' || t.kind == '#':
		return gqlValue{Raw: t.text}, nil
	}
	return gqlValue{}, errGQLSyntax
}

// gqlUnquote decodes the simple escapes. A \u escape is outside the subset.
func gqlUnquote(s string) (string, bool) {
	body := s[1 : len(s)-1]
	if !strings.Contains(body, `\`) {
		return body, true
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return "", false
		}
		switch body[i] {
		case '"', '\\', '/':
			b.WriteByte(body[i])
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		default:
			return "", false
		}
	}
	return b.String(), true
}
