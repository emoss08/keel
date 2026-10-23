package migrate

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Severity is how bad a Finding is.
type Severity uint8

const (
	// SeverityWarning is a risk to review, such as a lock held during a table scan.
	SeverityWarning Severity = iota
	// SeverityError is a migration that fails or takes the application down.
	SeverityError
)

// String returns "warning" or "error".
func (s Severity) String() string {
	if s == SeverityError {
		return "error"
	}
	return "warning"
}

// MarshalText encodes the severity as its name.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Finding is one problem Lint found.
type Finding struct {
	Rule      string
	Migration Migration
	Line      int
	Message   string
	Severity  Severity
}

// LintRule is a lint check. Check returns the findings for one migration; Lint fills in Rule and
// Migration when Check leaves them empty.
type LintRule struct {
	Name  string
	Check func(Migration) []Finding
}

// Lint checks migrations with the built-in rules, then rules, and returns the findings in
// migration and line order. A migration's --keel:lint_ignore directive skips the rules it names.
// The built-in rules analyse SQL lexically, with comments and literals removed, and treat a table
// not created earlier in the same migration as existing:
//
//	index-concurrently  CREATE INDEX without CONCURRENTLY on an existing table
//	not-null-default    ADD COLUMN … NOT NULL without a DEFAULT on an existing table
//	notx-mixed          CONCURRENTLY or VACUUM in a transaction, ALTER TYPE … ADD VALUE beside other
//	                    statements, or a notx statement sharing a --keel:split part with others
//	lock-heavy          ALTER COLUMN … TYPE, SET NOT NULL, ADD CONSTRAINT that validates or builds an
//	                    index under the table lock, VALIDATE in the migration that added it NOT VALID
func Lint(migrations []Migration, rules ...LintRule) []Finding {
	all := append(builtinRules(), rules...)
	var out []Finding
	for _, m := range migrations {
		var found []Finding
		for _, r := range all {
			if slices.Contains(m.Directives.LintIgnore, r.Name) {
				continue
			}
			for _, f := range r.Check(m) {
				if f.Rule == "" {
					f.Rule = r.Name
				}
				if f.Migration.Version == "" && f.Migration.Name == "" {
					f.Migration = m
				}
				found = append(found, f)
			}
		}
		slices.SortStableFunc(found, func(a, b Finding) int { return a.Line - b.Line })
		out = append(out, found...)
	}
	return out
}

// builtinRules returns the built-in rules.
func builtinRules() []LintRule {
	return []LintRule{
		{Name: "index-concurrently", Check: checkIndexConcurrently},
		{Name: "not-null-default", Check: checkNotNullDefault},
		{Name: "notx-mixed", Check: checkNoTxMixed},
		{Name: "lock-heavy", Check: checkLockHeavy},
	}
}

// sqlToken is one word or punctuation mark of a statement.
type sqlToken struct {
	text  string
	upper string
	off   int
}

// sqlStmt is one statement of a migration, tokenized.
type sqlStmt struct {
	toks []sqlToken
	part int
}

// is reports whether the tokens from i match words, case-insensitively.
func (s sqlStmt) is(i int, words ...string) bool {
	if i+len(words) > len(s.toks) {
		return false
	}
	for j, w := range words {
		if s.toks[i+j].upper != w {
			return false
		}
	}
	return true
}

// find returns the index of the first token from i matching word at parenthesis depth zero, or -1.
func (s sqlStmt) find(i int, word string) int {
	depth := 0
	for ; i < len(s.toks); i++ {
		switch s.toks[i].text {
		case "(":
			depth++
		case ")":
			depth--
		}
		if depth == 0 && s.toks[i].upper == word {
			return i
		}
	}
	return -1
}

// has reports whether the statement holds word anywhere.
func (s sqlStmt) has(word string) bool {
	return slices.ContainsFunc(s.toks, func(t sqlToken) bool { return t.upper == word })
}

// migrationSQL is a migration's statements with what it creates tracked.
type migrationSQL struct {
	m     Migration
	lex   lexed
	stmts []sqlStmt
}

// parseSQL tokenizes a migration's statements; parts count --keel:split lines before each.
func parseSQL(m Migration) migrationSQL {
	l := lex(m.SQL)
	var splits []int
	for _, ln := range l.lines {
		if ln.code && strings.TrimSpace(ln.text) == "--keel:split" {
			splits = append(splits, ln.start)
		}
	}
	out := migrationSQL{m: m, lex: l}
	for _, st := range l.statements() {
		part := 0
		for part < len(splits) && splits[part] < st.start {
			part++
		}
		if toks := tokenize(st.text, st.start); len(toks) > 0 {
			out.stmts = append(out.stmts, sqlStmt{toks: toks, part: part})
		}
	}
	return out
}

// tokenize splits masked statement text into words, quoted identifiers and parentheses and commas.
func tokenize(s string, base int) []sqlToken {
	var out []sqlToken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '(' || c == ')' || c == ',':
			out = append(out, sqlToken{text: s[i : i+1], upper: s[i : i+1], off: base + i})
			i++
		case identByte(c) || c == '"' || c == '.':
			j := wordEnd(s, i)
			out = append(out, sqlToken{text: s[i:j], upper: strings.ToUpper(s[i:j]), off: base + i})
			i = j
		default:
			i++
		}
	}
	return out
}

// wordEnd returns the end of the word starting at i, joining quoted parts and dots.
func wordEnd(s string, i int) int {
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			i = quotedEnd(s, i+1)
		case identByte(c) || c == '.':
			i++
		default:
			return i
		}
	}
	return i
}

// quotedEnd returns the position after the quote closing a quoted identifier whose content starts
// at i.
func quotedEnd(s string, i int) int {
	for i < len(s) {
		if s[i] != '"' {
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			i += 2
			continue
		}
		return i + 1
	}
	return i
}

// tableName normalizes a possibly qualified, possibly quoted name to its unqualified form.
func tableName(t string) string {
	parts := strings.Split(t, ".")
	last := parts[len(parts)-1]
	if strings.HasPrefix(last, `"`) {
		return strings.ReplaceAll(strings.Trim(last, `"`), `""`, `"`)
	}
	return strings.ToLower(last)
}

// created returns the tables each statement can treat as new: those created by earlier statements
// of the migration.
func (ms migrationSQL) created() []map[string]bool {
	out := make([]map[string]bool, len(ms.stmts))
	seen := map[string]bool{}
	for i, st := range ms.stmts {
		out[i] = maps.Clone(seen)
		if name, ok := createdTable(st); ok {
			seen[name] = true
		}
	}
	return out
}

// createdTable returns the table or materialized view a CREATE statement makes.
func createdTable(st sqlStmt) (string, bool) {
	if !st.is(0, "CREATE") {
		return "", false
	}
	i := 1
	for i < len(st.toks) && slices.Contains([]string{"OR", "REPLACE", "GLOBAL", "LOCAL", "TEMP", "TEMPORARY", "UNLOGGED", "MATERIALIZED"}, st.toks[i].upper) {
		i++
	}
	if !st.is(i, "TABLE") && !st.is(i, "VIEW") {
		return "", false
	}
	i++
	if st.is(i, "IF", "NOT", "EXISTS") {
		i += 3
	}
	if i >= len(st.toks) {
		return "", false
	}
	return tableName(st.toks[i].text), true
}

// finding returns a finding at token t of the migration.
func (ms migrationSQL) finding(t sqlToken, sev Severity, format string, args ...any) Finding {
	return Finding{Migration: ms.m, Line: ms.lex.line(t.off), Severity: sev, Message: fmt.Sprintf(format, args...)}
}

// checkIndexConcurrently flags CREATE INDEX without CONCURRENTLY on an existing table.
func checkIndexConcurrently(m Migration) []Finding {
	ms := parseSQL(m)
	news := ms.created()
	var out []Finding
	for i, st := range ms.stmts {
		at := 1
		if st.is(1, "UNIQUE") {
			at = 2
		}
		if !st.is(0, "CREATE") || !st.is(at, "INDEX") || st.is(at+1, "CONCURRENTLY") {
			continue
		}
		on := st.find(at+1, "ON")
		if on < 0 || on+1 >= len(st.toks) {
			continue
		}
		t := on + 1
		if st.is(t, "ONLY") {
			t++
		}
		if t >= len(st.toks) || news[i][tableName(st.toks[t].text)] {
			continue
		}
		out = append(out, ms.finding(st.toks[0], SeverityWarning,
			"CREATE INDEX on existing table %s blocks its writes while the index builds; use CREATE INDEX CONCURRENTLY in a --keel:notx migration",
			st.toks[t].text))
	}
	return out
}

// alterTable returns the table and the actions of an ALTER TABLE statement.
func alterTable(st sqlStmt) (string, [][]sqlToken, bool) {
	if !st.is(0, "ALTER", "TABLE") {
		return "", nil, false
	}
	i := 2
	if st.is(i, "IF", "EXISTS") {
		i += 2
	}
	if st.is(i, "ONLY") {
		i++
	}
	if i >= len(st.toks) {
		return "", nil, false
	}
	table := st.toks[i].text
	var actions [][]sqlToken
	depth, from := 0, i+1
	for j := i + 1; j <= len(st.toks); j++ {
		if j < len(st.toks) {
			switch st.toks[j].text {
			case "(":
				depth++
				continue
			case ")":
				depth--
				continue
			case ",":
				if depth > 0 {
					continue
				}
			default:
				continue
			}
		}
		if j > from {
			actions = append(actions, st.toks[from:j])
		}
		from = j + 1
	}
	return table, actions, true
}

// action wraps an ALTER TABLE action's tokens.
func action(toks []sqlToken) sqlStmt { return sqlStmt{toks: toks} }

// checkNotNullDefault flags ADD COLUMN … NOT NULL without a DEFAULT on an existing table.
func checkNotNullDefault(m Migration) []Finding {
	ms := parseSQL(m)
	news := ms.created()
	var out []Finding
	for i, st := range ms.stmts {
		table, actions, ok := alterTable(st)
		if !ok || news[i][tableName(table)] {
			continue
		}
		for _, toks := range actions {
			a := action(toks)
			col, ok := addedColumn(a)
			if !ok || a.find(0, "DEFAULT") >= 0 || a.find(0, "GENERATED") >= 0 || !notNull(a) {
				continue
			}
			out = append(out, ms.finding(toks[0], SeverityError,
				"ADD COLUMN %s NOT NULL without a DEFAULT fails on a table with rows; add a DEFAULT, or add it nullable, backfill it and set NOT NULL later",
				col))
		}
	}
	return out
}

// addedColumn returns the column an ADD [COLUMN] action adds.
func addedColumn(a sqlStmt) (string, bool) {
	if !a.is(0, "ADD") {
		return "", false
	}
	i := 1
	if a.is(i, "COLUMN") {
		i++
	} else if i < len(a.toks) && slices.Contains([]string{"CONSTRAINT", "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "EXCLUDE"}, a.toks[i].upper) {
		return "", false
	}
	if a.is(i, "IF", "NOT", "EXISTS") {
		i += 3
	}
	if i >= len(a.toks) {
		return "", false
	}
	return a.toks[i].text, true
}

// notNull reports whether an action holds NOT NULL at depth zero.
func notNull(a sqlStmt) bool {
	for i := a.find(0, "NOT"); i >= 0; i = a.find(i+1, "NOT") {
		if a.is(i+1, "NULL") {
			return true
		}
	}
	return false
}

// noTxOnly returns a description of the statement when Postgres refuses it inside a transaction.
func noTxOnly(st sqlStmt) (string, bool) {
	switch {
	case st.is(0, "VACUUM"):
		return "VACUUM", true
	case st.is(0, "REFRESH"), !st.has("CONCURRENTLY"):
		return "", false
	case st.is(0, "CREATE"):
		return "CREATE INDEX CONCURRENTLY", true
	case st.is(0, "DROP", "INDEX"):
		return "DROP INDEX CONCURRENTLY", true
	case st.is(0, "REINDEX"):
		return "REINDEX CONCURRENTLY", true
	case st.is(0, "ALTER", "TABLE") && st.has("DETACH"):
		return "DETACH PARTITION CONCURRENTLY", true
	}
	return "", false
}

// addsEnumValue reports whether the statement is ALTER TYPE … ADD VALUE.
func addsEnumValue(st sqlStmt) bool {
	return st.is(0, "ALTER", "TYPE") && st.find(2, "ADD") >= 0 && st.has("VALUE")
}

// checkNoTxMixed flags statements in the wrong kind of migration.
func checkNoTxMixed(m Migration) []Finding {
	ms := parseSQL(m)
	var out []Finding
	parts := map[int]int{}
	for _, st := range ms.stmts {
		parts[st.part]++
	}
	others := 0
	for _, st := range ms.stmts {
		if !addsEnumValue(st) {
			others++
		}
	}
	for _, st := range ms.stmts {
		what, only := noTxOnly(st)
		switch {
		case only && !m.Directives.NoTx:
			out = append(out, ms.finding(st.toks[0], SeverityError,
				"%s cannot run inside a transaction; put it in a --keel:notx migration", what))
		case only && parts[st.part] > 1:
			out = append(out, ms.finding(st.toks[0], SeverityError,
				"%s shares a --keel:split part with other statements, which Postgres runs as one transaction; separate them with --keel:split", what))
		case addsEnumValue(st) && !m.Directives.NoTx && others > 0:
			out = append(out, ms.finding(st.toks[0], SeverityWarning,
				"ALTER TYPE … ADD VALUE: the new value cannot be used until the migration commits; keep it in a migration of its own"))
		}
	}
	return out
}

// checkLockHeavy flags ALTER TABLE actions that hold an ACCESS EXCLUSIVE lock for a table rewrite
// or scan.
func checkLockHeavy(m Migration) []Finding {
	ms := parseSQL(m)
	news := ms.created()
	notValid := map[string]bool{}
	var out []Finding
	for i, st := range ms.stmts {
		table, actions, ok := alterTable(st)
		if !ok || news[i][tableName(table)] {
			continue
		}
		for _, toks := range actions {
			if f, ok := ms.lockHeavy(table, action(toks), notValid); ok {
				out = append(out, f)
			}
		}
	}
	return out
}

// lockHeavy checks one ALTER TABLE action, recording constraints added NOT VALID.
func (ms migrationSQL) lockHeavy(table string, a sqlStmt, notValid map[string]bool) (Finding, bool) {
	t := a.toks[0]
	switch {
	case a.is(0, "ALTER"):
		col := 1
		if a.is(1, "COLUMN") {
			col = 2
		}
		switch {
		case a.is(col+1, "TYPE") || a.is(col+1, "SET", "DATA", "TYPE"):
			return ms.finding(t, SeverityWarning,
				"changing a column's type on %s rewrites the table under an ACCESS EXCLUSIVE lock; add a new column, backfill it and swap", table), true
		case a.is(col+1, "SET", "NOT", "NULL"):
			return ms.finding(t, SeverityWarning,
				"SET NOT NULL on %s scans the table under an ACCESS EXCLUSIVE lock; add CHECK (… IS NOT NULL) NOT VALID, validate it in a later migration, then set NOT NULL", table), true
		}
	case a.is(0, "ADD") && (a.has("FOREIGN") || a.has("CHECK") || a.has("UNIQUE") || a.has("PRIMARY")):
		if _, isColumn := addedColumn(a); isColumn {
			return Finding{}, false
		}
		if a.is(1, "CONSTRAINT") && len(a.toks) > 2 && a.find(0, "NOT") >= 0 && a.has("VALID") {
			notValid[tableName(a.toks[2].text)] = true
		}
		switch {
		case (a.has("UNIQUE") || a.has("PRIMARY")) && !a.has("USING"):
			return ms.finding(t, SeverityWarning,
				"adding a unique or primary key constraint to %s builds its index under an ACCESS EXCLUSIVE lock; CREATE UNIQUE INDEX CONCURRENTLY first, then ADD CONSTRAINT … USING INDEX", table), true
		case (a.has("FOREIGN") || a.has("CHECK")) && !a.has("VALID"):
			return ms.finding(t, SeverityWarning,
				"this constraint validates every row of %s while holding its lock; add it NOT VALID and VALIDATE CONSTRAINT in a later migration", table), true
		}
	case a.is(0, "VALIDATE", "CONSTRAINT") && len(a.toks) > 2 && notValid[tableName(a.toks[2].text)]:
		return ms.finding(t, SeverityWarning,
			"VALIDATE CONSTRAINT %s in the migration that added it NOT VALID holds the lock for the whole scan; validate it in a later migration", a.toks[2].text), true
	}
	return Finding{}, false
}
