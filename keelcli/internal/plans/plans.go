// Package plans is keelcli's plan guard: it explains recorded statement shapes with
// EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON) and checks every plan against the rules, so a
// query that would scan a large table, cost too much or sort a page without an index is caught
// before it ships.
package plans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/emoss08/keel"
)

// MinServerVersion is the first server_version_num with EXPLAIN (GENERIC_PLAN): PostgreSQL 16.
const MinServerVersion = 160000

// Rules are the checks a plan must pass.
type Rules struct {
	// SeqScanRows flags a sequential scan of a relation whose pg_class.reltuples exceeds it; a
	// relation never analyzed counts as empty, and a negative value flags every sequential scan.
	SeqScanRows float64
	// MaxCost flags a plan whose total cost exceeds it; 0 or less checks no cost.
	MaxCost float64
	// SortLimit flags a Sort under a Limit: a page sorted in full because no index gives its order.
	SortLimit bool
}

// Kind names a finding.
type Kind string

// The findings.
const (
	SeqScan        Kind = "seq-scan"
	Cost           Kind = "cost"
	SortUnderLimit Kind = "sort-under-limit"
)

// Finding is one rule a plan breaks. Relation is schema.name for a sequential scan, Rows its
// reltuples; Cost is the plan's total cost for a cost finding; SortKey is the sort's key for a
// sort under a limit.
type Finding struct {
	Kind     Kind     `json:"kind"`
	Relation string   `json:"relation,omitempty"`
	Rows     float64  `json:"rows,omitempty"`
	Cost     float64  `json:"cost,omitempty"`
	SortKey  []string `json:"sort_key,omitempty"`
	Detail   string   `json:"detail"`
}

// Report is one shape's result: the plan's total cost and row estimate and its findings, or the
// error EXPLAIN returned when it could not plan the shape.
type Report struct {
	keel.Shape
	Cost     float64   `json:"cost"`
	Rows     float64   `json:"rows"`
	Findings []Finding `json:"findings"`
	Error    string    `json:"error,omitempty"`
}

// Planned reports whether EXPLAIN planned the shape.
func (r Report) Planned() bool { return r.Error == "" }

// Node is one node of an EXPLAIN (FORMAT JSON) plan, with the fields the rules read.
type Node struct {
	Type      string   `json:"Node Type"`
	Relation  string   `json:"Relation Name"`
	Schema    string   `json:"Schema"`
	TotalCost float64  `json:"Total Cost"`
	PlanRows  float64  `json:"Plan Rows"`
	SortKey   []string `json:"Sort Key"`
	Plans     []Node   `json:"Plans"`
}

// Relation is a table, materialized view or partition a plan reads.
type Relation struct {
	Schema string
	Name   string
}

// String returns schema.name, or the name alone without a schema.
func (r Relation) String() string {
	if r.Schema == "" {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

// Parse reads EXPLAIN (FORMAT JSON) output and returns its top plan node.
func Parse(raw []byte) (Node, error) {
	var out []struct {
		Plan *Node `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Node{}, fmt.Errorf("keelcli: parse EXPLAIN output: %w", err)
	}
	if len(out) == 0 || out[0].Plan == nil {
		return Node{}, errors.New("keelcli: EXPLAIN returned no plan")
	}
	return *out[0].Plan, nil
}

// walk calls fn for n and every node below it, parents first, init plans and subplans included.
func (n Node) walk(fn func(Node)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// SeqScans returns the relations the plan reads with a sequential scan, parallel ones included,
// in plan order, each once.
func (n Node) SeqScans() []Relation {
	var out []Relation
	n.walk(func(x Node) {
		if x.Type != "Seq Scan" || x.Relation == "" {
			return
		}
		r := Relation{Schema: x.Schema, Name: x.Relation}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	})
	return out
}

// passThrough are the nodes between a Limit and a Sort that keep the Sort's full input on the
// Limit's path: the Limit still waits for the whole sort.
var passThrough = []string{"Gather", "Gather Merge", "Result", "Subquery Scan", "Materialize", "Unique", "Merge Append"}

// SortsUnderLimit returns the Sort nodes a Limit reads through pass-through nodes only: pages
// sorted in full. An Incremental Sort, which an index orders by its leading keys, is not one.
func (n Node) SortsUnderLimit() []Node {
	var out []Node
	n.walk(func(x Node) {
		if x.Type == "Limit" {
			for _, c := range x.Plans {
				out = append(out, sortsBelow(c)...)
			}
		}
	})
	return out
}

// sortsBelow returns n when it is a Sort, else the Sorts below n through pass-through nodes.
func sortsBelow(n Node) []Node {
	if n.Type == "Sort" {
		return []Node{n}
	}
	if !slices.Contains(passThrough, n.Type) {
		return nil
	}
	var out []Node
	for _, c := range n.Plans {
		out = append(out, sortsBelow(c)...)
	}
	return out
}

// sortedInFull explains a sort-under-limit finding.
const sortedInFull = ": the page is sorted in full, no index gives its order"

// Check returns the rules root breaks. tuples holds each relation's reltuples; a relation
// missing from it, or with a negative count (never analyzed), counts as empty.
func Check(root Node, rules Rules, tuples map[Relation]float64) []Finding {
	var out []Finding
	for _, r := range root.SeqScans() {
		rows := max(tuples[r], 0)
		if rows <= rules.SeqScanRows {
			continue
		}
		out = append(out, Finding{Kind: SeqScan, Relation: r.String(), Rows: rows,
			Detail: fmt.Sprintf("sequential scan of %s (about %s rows)", r, formatCount(rows))})
	}
	if rules.MaxCost > 0 && root.TotalCost > rules.MaxCost {
		out = append(out, Finding{Kind: Cost, Cost: root.TotalCost,
			Detail: fmt.Sprintf("total cost %s over %s", formatCost(root.TotalCost), formatCost(rules.MaxCost))})
	}
	if rules.SortLimit {
		for _, s := range root.SortsUnderLimit() {
			detail := "sort under a limit" + sortedInFull
			if len(s.SortKey) > 0 {
				detail = "sort by " + strings.Join(s.SortKey, ", ") + " under a limit" + sortedInFull
			}
			out = append(out, Finding{Kind: SortUnderLimit, SortKey: s.SortKey, Detail: detail})
		}
	}
	return out
}

// formatCount writes a row count with thousands separators.
func formatCount(n float64) string {
	s := strconv.FormatInt(int64(n), 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// formatCost writes a planner cost as EXPLAIN prints it, two decimals at most.
func formatCost(c float64) string { return strconv.FormatFloat(c, 'f', -1, 64) }

// SingleStatement reports whether sql is one statement: no semicolon outside string literals,
// quoted identifiers, dollar quotes and comments, but a trailing one. The simple query protocol
// EXPLAIN goes over runs every statement of a text, so a shape is explained only when it is one.
func SingleStatement(sql string) bool {
	for i := 0; i < len(sql); i++ {
		switch c := sql[i]; {
		case c == '\'':
			i = skipQuoted(sql, i, '\'', i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e'))
		case c == '"':
			i = skipQuoted(sql, i, '"', false)
		case c == '-' && strings.HasPrefix(sql[i:], "--"):
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				return true
			}
			i += j
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			i = skipBlockComment(sql, i)
		case c == '$':
			i = skipDollarQuote(sql, i)
		case c == ';':
			return strings.TrimSpace(sql[i+1:]) == ""
		}
		if i < 0 {
			return false
		}
	}
	return true
}

// skipQuoted returns the index of the quote that closes the literal or identifier opened at
// sql[i], with a doubled quote standing for itself and, in an E-prefixed string, a backslash
// escaping the next byte; -1 when it never closes.
func skipQuoted(sql string, i int, quote byte, backslash bool) int {
	for j := i + 1; j < len(sql); j++ {
		switch {
		case backslash && sql[j] == '\\':
			j++
		case sql[j] == quote && j+1 < len(sql) && sql[j+1] == quote:
			j++
		case sql[j] == quote:
			return j
		}
	}
	return -1
}

// skipBlockComment returns the index of the last byte of the block comment, nested ones
// included, opened at sql[i]; -1 when it never closes.
func skipBlockComment(sql string, i int) int {
	depth := 0
	for j := i; j+1 < len(sql); j++ {
		switch {
		case sql[j] == '/' && sql[j+1] == '*':
			depth++
			j++
		case sql[j] == '*' && sql[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// skipDollarQuote returns the index of the last byte of the dollar-quoted string opened at
// sql[i] ($$...$$ or $tag$...$tag$), i itself when sql[i] starts a parameter ($1) or nothing,
// and -1 when the string never closes.
func skipDollarQuote(sql string, i int) int {
	if i > 0 && identByte(sql[i-1]) {
		return i
	}
	j := i + 1
	for j < len(sql) && identByte(sql[j]) && (j > i+1 || sql[j] < '0' || sql[j] > '9') {
		j++
	}
	if j >= len(sql) || sql[j] != '$' {
		return i
	}
	tag := sql[i : j+1]
	end := strings.Index(sql[j+1:], tag)
	if end < 0 {
		return -1
	}
	return j + 1 + end + len(tag) - 1
}

// identByte reports whether c can be part of an unquoted identifier or a dollar quote's tag.
func identByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// Run explains every shape on conn, outside transactions, and checks each plan against rules. A
// shape EXPLAIN cannot plan gets a report with its error; a server older than PostgreSQL 16, a
// lost connection or a canceled context ends the run with an error. The reltuples of every
// relation scanned sequentially are read in one statement after the plans.
func Run(ctx context.Context, conn *pgx.Conn, shapes []keel.Shape, rules Rules) ([]Report, error) {
	if err := checkVersion(ctx, conn); err != nil {
		return nil, err
	}
	reports := make([]Report, len(shapes))
	roots := make([]*Node, len(shapes))
	var scanned []Relation
	for i, s := range shapes {
		reports[i] = Report{Shape: s, Findings: []Finding{}}
		root, err := explain(ctx, conn, s.SQL)
		var pgErr *pgconn.PgError
		var u unplanned
		switch {
		case errors.As(err, &pgErr):
			reports[i].Error = pgErr.Message + " (SQLSTATE " + pgErr.Code + ")"
			continue
		case errors.As(err, &u):
			reports[i].Error = u.Error()
			continue
		case err != nil:
			return nil, err
		}
		roots[i] = &root
		reports[i].Cost, reports[i].Rows = root.TotalCost, root.PlanRows
		for _, r := range root.SeqScans() {
			if !slices.Contains(scanned, r) {
				scanned = append(scanned, r)
			}
		}
	}
	tuples, err := relTuples(ctx, conn, scanned)
	if err != nil {
		return nil, err
	}
	for i, root := range roots {
		if root != nil {
			reports[i].Findings = append(reports[i].Findings, Check(*root, rules, tuples)...)
		}
	}
	return reports, nil
}

// unplanned is the error of a shape EXPLAIN was not asked to plan or returned no plan for; Run
// reports it and goes on.
type unplanned string

// Error returns the reason.
func (u unplanned) Error() string { return string(u) }

// checkVersion refuses a server without EXPLAIN (GENERIC_PLAN).
func checkVersion(ctx context.Context, conn *pgx.Conn) error {
	var v string
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')").Scan(&v); err != nil {
		return fmt.Errorf("keelcli: read the server version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("keelcli: read the server version %q: %w", v, err)
	}
	if n < MinServerVersion {
		return fmt.Errorf("keelcli: plans needs PostgreSQL 16 or later for EXPLAIN (GENERIC_PLAN); the server is %d", n)
	}
	return nil
}

// explain runs EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON) of sql over the simple query
// protocol, which leaves its placeholders unbound, and parses the plan. VERBOSE names each
// relation's schema.
func explain(ctx context.Context, conn *pgx.Conn, sql string) (Node, error) {
	if !SingleStatement(sql) {
		return Node{}, unplanned("the shape holds more than one statement")
	}
	results, err := conn.PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON) "+sql).ReadAll()
	if err != nil {
		return Node{}, err
	}
	if len(results) == 0 || len(results[0].Rows) == 0 || len(results[0].Rows[0]) == 0 {
		return Node{}, unplanned("EXPLAIN returned no plan")
	}
	root, err := Parse(results[0].Rows[0][0])
	if err != nil {
		return Node{}, unplanned(err.Error())
	}
	return root, nil
}

// relTuples returns pg_class.reltuples of each relation, in one statement.
func relTuples(ctx context.Context, conn *pgx.Conn, rels []Relation) (map[Relation]float64, error) {
	out := make(map[Relation]float64, len(rels))
	if len(rels) == 0 {
		return out, nil
	}
	schemas, names := make([]string, len(rels)), make([]string, len(rels))
	for i, r := range rels {
		schemas[i], names[i] = r.Schema, r.Name
	}
	rows, err := conn.Query(ctx, `SELECT n.nspname, c.relname, c.reltuples::float8
FROM unnest($1::text[], $2::text[]) AS r (nsp, rel)
JOIN pg_namespace n ON n.nspname = r.nsp
JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = r.rel`, schemas, names)
	if err != nil {
		return nil, fmt.Errorf("keelcli: read relation sizes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Relation
		var n float64
		if err := rows.Scan(&r.Schema, &r.Name, &n); err != nil {
			return nil, fmt.Errorf("keelcli: read relation sizes: %w", err)
		}
		out[r] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keelcli: read relation sizes: %w", err)
	}
	return out, nil
}
