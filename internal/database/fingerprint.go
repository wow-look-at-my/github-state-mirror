package database

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// currentFingerprint is schemaFingerprint(schemaSQL), computed. The input
var currentFingerprint = sync.OnceValue(func() string { return schemaFingerprint(schemaSQL) })

// schemaFingerprint reduces schema.sql to the tables it actually builds --
func schemaFingerprint(schema string) string {
	sum := sha256.Sum256([]byte(scrubSQL(schema)))
	return hex.EncodeToString(sum[:])
}

// scrubSQL strips SQL comments and normalizes formatting: a run of whitespace
// or comments between tokens becomes a single space, dropped entirely when
// either side is self-delimiting punctuation, so re-wrapping a CREATE TABLE
// reads as the same schema.
func scrubSQL(sql string) string {
	var out scrubbed

	for i := 0; i < len(sql); {
		switch c := sql[i]; {
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			out.gap()

		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i += 2
			for i < len(sql) && !(sql[i] == '*' && i+1 < len(sql) && sql[i+1] == '/') {
				i++
			}
			i = min(i+2, len(sql))
			out.gap()

		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			out.gap()

		case c == '\'' || c == '"' || c == '`':
			end := endOfQuoted(sql, i, c)
			out.write(sql[i:end])
			i = end

		case c == '[':
			end := endOfQuoted(sql, i, ']')
			out.write(sql[i:end])
			i = end

		default:
			out.write(sql[i : i+1])
			i++
		}
	}

	return out.b.String()
}

// scrubbed accumulates the normalized text, holding each gap open until it
// knows what follows it.
type scrubbed struct {
	b       strings.Builder
	last    byte
	pending bool
}

func (s *scrubbed) gap() {
	s.pending = s.b.Len() > 0
}

func (s *scrubbed) write(tok string) {
	if s.pending {
		if !selfDelimiting(s.last) && !selfDelimiting(tok[0]) {
			s.b.WriteByte(' ')
		}
		s.pending = false
	}
	s.b.WriteString(tok)
	s.last = tok[len(tok)-1]
}

// selfDelimiting reports whether a character separates tokens by itself, so
func selfDelimiting(c byte) bool {
	return c == '(' || c == ')' || c == ',' || c == ';'
}

// endOfQuoted returns the index just past the quoted run opening at sql[start].
// A doubled closing quote is an escaped and does not end the run; an
func endOfQuoted(sql string, start int, closer byte) int {
	for i := start + 1; i < len(sql); i++ {
		if sql[i] != closer {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == closer {
			i++
			continue
		}
		return i + 1
	}
	return len(sql)
}
