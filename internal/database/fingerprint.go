package database

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// currentFingerprint is schemaFingerprint(schemaSQL), computed once. The input
// is a compiled-in constant, so re-scrubbing 40 KB of DDL on every Open buys
// nothing.
var currentFingerprint = sync.OnceValue(func() string { return schemaFingerprint(schemaSQL) })

// schemaFingerprint reduces schema.sql to the tables it actually builds --
// comments dropped, formatting normalized -- and hashes the result. That hash
// is the entire nuke decision: a DB whose recorded fingerprint differs from
// this binary's describes different tables, so it is deleted and rebuilt.
// Nothing else nukes, and nobody has to remember to declare that a schema
// changed.
//
// Normalizing is what makes the decision honest in both directions. Hashing
// the raw text would rebuild a whole fleet's cache over a reworded column
// comment -- two thirds of the lines in schema.sql carry one -- and a nuke
// that fires on prose is one nobody trusts. Dropping too much is the worse
// direction: if two genuinely different schemas normalized alike, a new column
// would deploy against an old table and every write naming it would fail,
// which is the breakage this mechanism exists to make impossible. So the two
// places bytes are load-bearing are left exactly as written: a quoted run
// (string literal or quoted identifier) is copied verbatim, spacing included,
// and a gap between two ordinary tokens is preserved as one space.
func schemaFingerprint(schema string) string {
	sum := sha256.Sum256([]byte(scrubSQL(schema)))
	return hex.EncodeToString(sum[:])
}

// scrubSQL strips SQL comments and normalizes formatting: a run of whitespace
// or comments between two tokens becomes a single space, dropped entirely when
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
// whitespace beside it carries no meaning. Deliberately only the four that are
// unambiguous in DDL: dropping a space next to any of them can never fuse two
// identifiers into one.
func selfDelimiting(c byte) bool {
	return c == '(' || c == ')' || c == ',' || c == ';'
}

// endOfQuoted returns the index just past the quoted run opening at sql[start].
// A doubled closing quote is an escaped one and does not end the run; an
// unterminated run runs to end of input, which is a schema that will not parse
// anyway.
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
