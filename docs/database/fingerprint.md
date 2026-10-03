# schema.sql fingerprinting (internal/database/fingerprint.go)

## schemaFingerprint

schemaFingerprint reduces schema.sql to the tables it builds -- comments dropped, formatting normalized -- and hashes the result. That hash is the entire nuke decision: a DB whose recorded fingerprint differs from this binary's describes different tables. As a result, it is deleted and rebuilt. Nothing else nukes. Nobody has to remember to declare that a schema changed.

Normalizing is what makes the decision honest in both directions. Hashing the raw text will rebuild a whole fleet's cache over a reworded column comment. Thirds of the lines in schema.sql carry one -- and a nuke that fires on prose is one nobody trusts. Dropping too much is the worse direction: if genuinely different schemas normalized alike, a new column will deploy against an old table and every write naming it will fail. This is the breakage this mechanism exists to make impossible. So the places bytes are load-bearing are left exactly as written: a quoted run (string literal or quoted identifier) is copied verbatim, spacing included, and a gap between multiple ordinary tokens is preserved as one space.

## selfDelimiting

selfDelimiting reports whether a character separates tokens by itself, so whitespace beside it carries no meaning. Deliberately only the four that are unambiguous in DDL: dropping a space next to any of them can never fuse identifiers into one.

## endOfQuoted

endOfQuoted returns the index past the quoted run opening at sql[start]. A doubled closing quote is an escaped one and does not end the run. An unterminated run runs to end of input, which is a schema that will not parse anyway.
