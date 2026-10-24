// Package diff compares two runs of the same report: which rows appeared, disappeared or
// moved, and by how much.
//
// Rows are matched by key, not by position, so a re-sorted report has not changed. Measures are
// compared as exact decimals, so a cent never goes missing through a float. A key that repeats
// within either run is reported once as a duplicate and never compared, because its numbers
// were never one row's. Only detail rows take part: subtotal and total rows are derived from
// them.
//
// Either side can be a live result or a stored run read back with export.ReadJSON:
//
//	before, _ := export.ReadJSON(lastWeek)
//	after, _ := export.ReadJSON(thisWeek)
//	res, err := diff.Compare(ctx, before.Table.Reader(), after.Table.Reader(), diff.Options{})
package diff

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/emoss08/keel/analytics/table"
	"github.com/emoss08/keel/analytics/value"
)

// Kind is what happened to a row between the two runs.
type Kind string

// The kinds of change, in the order a reader checks them.
const (
	// Duplicate is a key that repeats within a run, so its rows cannot be compared.
	Duplicate Kind = "duplicate"
	// Removed is a row only the earlier run has.
	Removed Kind = "removed"
	// Added is a row only the later run has.
	Added Kind = "added"
	// Changed is a row both runs have with different measures.
	Changed Kind = "changed"
	// Unchanged is a row both runs have with the same measures.
	Unchanged Kind = "unchanged"
)

// DefaultLimit is the number of changes a result lists when Options.Limit is zero.
const DefaultLimit = 200

// The errors Compare returns.
var (
	// ErrSchemaMismatch reports runs whose columns differ in number, ID or type.
	ErrSchemaMismatch = errors.New("diff: the two runs do not have the same columns")
	// ErrNoKeys reports runs with no column to identify a row by.
	ErrNoKeys = errors.New("diff: the runs have no column that identifies a row")
	// ErrNoMeasures reports runs with no numeric column to compare.
	ErrNoMeasures = errors.New("diff: the runs have no numeric column to compare")
	// ErrUnknownColumn reports a key or measure that names no column.
	ErrUnknownColumn = errors.New("diff: no such column in these runs")
)

// Options narrow a comparison.
type Options struct {
	// Keys are the IDs of the columns that identify a row. Empty means every string, enum,
	// reference, boolean and instant column.
	Keys []string
	// Measures are the IDs of the columns compared. Empty means every int, decimal and float
	// column.
	Measures []string
	// IncludeUnchanged lists unchanged rows too; they are always counted in the summary.
	IncludeUnchanged bool
	// Limit bounds the changes listed, after ranking; zero is DefaultLimit and a negative
	// limit lists every change. The summary always counts every row.
	Limit int
}

// Result is a comparison of two runs.
type Result struct {
	// Keys are the IDs of the key columns.
	Keys []string `json:"keys"`
	// Measures are the IDs of the compared columns.
	Measures []string `json:"measures"`
	// Summary counts the rows by kind of change.
	Summary Summary `json:"summary"`
	// Changes lists the changes, most worth reading first.
	Changes []Change `json:"changes"`
	// Totals sums each measure over the rows present on each side.
	Totals []Measure `json:"totals"`
	// Truncated reports that a run was cut at a row limit, so a removed row may only be past
	// the limit.
	Truncated bool `json:"truncated"`
}

// Summary counts rows by kind of change.
type Summary struct {
	// Added counts the rows only the later run has.
	Added int `json:"added"`
	// Removed counts the rows only the earlier run has.
	Removed int `json:"removed"`
	// Changed counts the rows whose measures moved.
	Changed int `json:"changed"`
	// Unchanged counts the rows that restate themselves.
	Unchanged int `json:"unchanged"`
	// Duplicate counts the keys that repeat in either run.
	Duplicate int `json:"duplicate"`
}

// Change is what happened to one row.
type Change struct {
	// Kind is what happened.
	Kind Kind `json:"kind"`
	// Key is the row's key, one value per key column; a null key cell is "".
	Key []string `json:"key"`
	// Measures are the measures that moved, or for an added or removed row every measure
	// against zero.
	Measures  []Measure `json:"measures,omitempty"`
	magnitude pgtype.Numeric
}

// Measure is one measure before and after, as exact decimals without trailing zeros.
type Measure struct {
	// Column is the measure's column ID.
	Column string `json:"column"`
	// Label is the column's label.
	Label string `json:"label"`
	// Before is the earlier value.
	Before string `json:"before"`
	// After is the later value.
	After string `json:"after"`
	// Delta is After minus Before.
	Delta string `json:"delta"`
}

// Compare reads both runs to the end and reports what changed from before to after.
func Compare(ctx context.Context, before, after table.Reader, opts Options) (*Result, error) {
	cols := after.Columns()
	if err := sameShape(before.Columns(), cols); err != nil {
		return nil, err
	}
	keys, err := resolve(cols, opts.Keys, isKey, ErrNoKeys)
	if err != nil {
		return nil, err
	}
	measures, err := resolve(cols, opts.Measures, func(t value.Type) bool { return t.Numeric() }, ErrNoMeasures)
	if err != nil {
		return nil, err
	}
	c := &comparison{cols: cols, keys: keys, measures: measures}
	prior, err := c.read(ctx, before)
	if err != nil {
		return nil, err
	}
	current, err := c.read(ctx, after)
	if err != nil {
		return nil, err
	}
	res := &Result{Keys: ids(cols, keys), Measures: ids(cols, measures),
		Truncated: before.Truncated() || after.Truncated()}
	changes := c.changes(prior, current)
	res.Summary = summarize(changes)
	res.Totals = c.totals(prior, current)
	res.Changes = rank(changes, opts)
	return res, nil
}

// sameShape refuses runs whose columns differ: one added column shifts every position after it.
func sameShape(before, after []table.Column) error {
	if len(before) != len(after) {
		return fmt.Errorf("%w: the earlier run has %d columns and the later one has %d",
			ErrSchemaMismatch, len(before), len(after))
	}
	for i := range after {
		switch {
		case before[i].ID != after[i].ID:
			return fmt.Errorf("%w: column %d is %q in the earlier run and %q in the later one",
				ErrSchemaMismatch, i+1, before[i].ID, after[i].ID)
		case before[i].Type != after[i].Type:
			return fmt.Errorf("%w: column %q is %s in the earlier run and %s in the later one",
				ErrSchemaMismatch, after[i].ID, before[i].Type, after[i].Type)
		}
	}
	return nil
}

// isKey reports whether a column of type t identifies a row by default. JSON has no reliable
// equality and numbers are measures.
func isKey(t value.Type) bool {
	return t.Textual() || t == value.Bool || t.Instant()
}

// resolve turns chosen column IDs into positions, or picks the columns of a fitting type when none
// are chosen, and returns none when that leaves nothing.
func resolve(cols []table.Column, chosen []string, fits func(value.Type) bool, none error) ([]int, error) {
	var out []int
	if len(chosen) == 0 {
		for i, c := range cols {
			if fits(c.Type) {
				out = append(out, i)
			}
		}
	}
	for _, id := range chosen {
		at := slices.IndexFunc(cols, func(c table.Column) bool { return c.ID == id })
		if at < 0 {
			return nil, fmt.Errorf("%w: %q", ErrUnknownColumn, id)
		}
		out = append(out, at)
	}
	if len(out) == 0 {
		return nil, none
	}
	return out, nil
}

// ids returns the IDs of the columns at the given positions.
func ids(cols []table.Column, at []int) []string {
	out := make([]string, len(at))
	for i, c := range at {
		out[i] = cols[c].ID
	}
	return out
}

// comparison is one Compare call: the schema, the key and measure positions and a scratch buffer.
type comparison struct {
	cols     []table.Column
	keys     []int
	measures []int
	scratch  []byte
}

// row is one detail row as a comparison keeps it.
type row struct {
	key      string
	values   []string
	measures []pgtype.Numeric
}

// side is one run's detail rows in order, indexed by key, with its repeated keys and sums.
type side struct {
	rows  []row
	index map[string]int
	dups  map[string]bool
	sums  []pgtype.Numeric
}

// read collects a run's detail rows.
func (c *comparison) read(ctx context.Context, r table.Reader) (*side, error) {
	s := &side{index: map[string]int{}, dups: map[string]bool{}, sums: make([]pgtype.Numeric, len(c.measures))}
	for i := range s.sums {
		s.sums[i] = value.NumericFromInt(0)
	}
	for {
		b, err := r.Next(ctx)
		if errors.Is(err, io.EOF) {
			return s, nil
		}
		if err != nil {
			return nil, err
		}
		for i := range b.Len {
			if b.Level(i) == table.Detail {
				s.add(c.row(b, i))
			}
		}
	}
}

// add adds a row to its run's sums, and records it under its key unless the key repeats.
func (s *side) add(r row) {
	for i, m := range r.measures {
		s.sums[i], _ = value.AddNumeric(s.sums[i], m)
	}
	if _, seen := s.index[r.key]; seen {
		s.dups[r.key] = true
		return
	}
	s.index[r.key] = len(s.rows)
	s.rows = append(s.rows, r)
}

// row reads row i of b: its key, encoded so that no two different keys collide (each cell is a
// null flag and a length-prefixed text), its key values and its measures.
func (c *comparison) row(b *table.Batch, i int) row {
	r := row{values: make([]string, len(c.keys)), measures: make([]pgtype.Numeric, len(c.measures))}
	c.scratch = c.scratch[:0]
	for k, col := range c.keys {
		v := &b.Columns[col]
		if v.Null(i) {
			c.scratch = append(c.scratch, 0)
			continue
		}
		r.values[k] = keyText(c.cols[col].Type, v, i)
		c.scratch = append(c.scratch, 1)
		c.scratch = binary.AppendUvarint(c.scratch, uint64(len(r.values[k])))
		c.scratch = append(c.scratch, r.values[k]...)
	}
	r.key = string(c.scratch)
	for m, col := range c.measures {
		r.measures[m] = measureOf(c.cols[col].Type, &b.Columns[col], i)
	}
	return r
}

// keyText is a key cell's text: decimals without trailing zeros, instants in RFC 3339 UTC.
func keyText(t value.Type, v *value.Vector, i int) string {
	switch t {
	case value.Int, value.Epoch:
		return strconv.FormatInt(v.Int64[i], 10)
	case value.Decimal:
		return string(value.AppendNumeric(nil, value.NormalizeNumeric(v.Numeric[i])))
	case value.Float:
		return strconv.FormatFloat(v.Float64[i], 'g', -1, 64)
	case value.Bool:
		return strconv.FormatBool(v.Bool[i])
	case value.Time:
		return v.Time[i].UTC().Format(time.RFC3339Nano)
	case value.JSON:
		return string(v.JSON[i])
	}
	return v.String[i]
}

// measureOf reads a measure exactly. A null, NaN or infinite measure counts as zero: it adds
// nothing to a sum.
func measureOf(t value.Type, v *value.Vector, i int) pgtype.Numeric {
	if v.Null(i) {
		return value.NumericFromInt(0)
	}
	var n pgtype.Numeric
	switch t {
	case value.Int, value.Epoch:
		n = value.NumericFromInt(v.Int64[i])
	case value.Decimal:
		n = v.Numeric[i]
	case value.Float:
		n = value.NumericFromFloat(v.Float64[i])
	}
	if !value.NumericFinite(n) {
		return value.NumericFromInt(0)
	}
	return n
}

// changes classifies every key of both runs: tainted keys once as duplicates, the later run's rows
// as added, changed or unchanged, then the earlier run's unmatched rows as removed.
func (c *comparison) changes(prior, current *side) []Change {
	var out []Change
	tainted := map[string]bool{}
	for _, s := range []*side{prior, current} {
		for _, r := range s.rows {
			if s.dups[r.key] && !tainted[r.key] {
				tainted[r.key] = true
				out = append(out, Change{Kind: Duplicate, Key: r.values})
			}
		}
	}
	for _, r := range current.rows {
		if tainted[r.key] {
			continue
		}
		at, ok := prior.index[r.key]
		if !ok {
			out = append(out, c.against(Added, r))
			continue
		}
		out = append(out, c.compare(prior.rows[at], r))
	}
	for _, r := range prior.rows {
		if _, kept := current.index[r.key]; !kept && !tainted[r.key] {
			out = append(out, c.against(Removed, r))
		}
	}
	return out
}

// against states an added or removed row's measures against zero, so a row that appeared reads
// like one that grew from nothing.
func (c *comparison) against(kind Kind, r row) Change {
	ch := Change{Kind: kind, Key: r.values, magnitude: value.NumericFromInt(0)}
	zero := value.NumericFromInt(0)
	for m, col := range c.measures {
		before, after := zero, r.measures[m]
		if kind == Removed {
			before, after = after, zero
		}
		ch.add(c.cols[col], before, after)
	}
	return ch
}

// compare states the measures that moved between a row's two versions.
func (c *comparison) compare(prior, current row) Change {
	ch := Change{Kind: Unchanged, Key: current.values, magnitude: value.NumericFromInt(0)}
	for m, col := range c.measures {
		if same, _ := value.CompareNumeric(prior.measures[m], current.measures[m]); same == 0 {
			continue
		}
		ch.Kind = Changed
		ch.add(c.cols[col], prior.measures[m], current.measures[m])
	}
	return ch
}

// add records a measure's move and widens the change's magnitude to it.
func (ch *Change) add(col table.Column, before, after pgtype.Numeric) {
	delta, _ := value.SubNumeric(after, before)
	ch.Measures = append(ch.Measures, Measure{Column: col.ID, Label: col.Label,
		Before: text(before), After: text(after), Delta: text(delta)})
	size := delta
	if size.Int.Sign() < 0 {
		size.Int.Neg(size.Int)
	}
	if more, _ := value.CompareNumeric(size, ch.magnitude); more > 0 {
		ch.magnitude = size
	}
}

// text writes a decimal without trailing zeros.
func text(n pgtype.Numeric) string {
	return string(value.AppendNumeric(nil, value.NormalizeNumeric(n)))
}

// totals sums each measure over the rows present on each side.
func (c *comparison) totals(prior, current *side) []Measure {
	out := make([]Measure, len(c.measures))
	for m, col := range c.measures {
		delta, _ := value.SubNumeric(current.sums[m], prior.sums[m])
		out[m] = Measure{Column: c.cols[col].ID, Label: c.cols[col].Label,
			Before: text(prior.sums[m]), After: text(current.sums[m]), Delta: text(delta)}
	}
	return out
}

// summarize counts the changes by kind.
func summarize(changes []Change) Summary {
	var s Summary
	for _, ch := range changes {
		switch ch.Kind {
		case Added:
			s.Added++
		case Removed:
			s.Removed++
		case Changed:
			s.Changed++
		case Unchanged:
			s.Unchanged++
		case Duplicate:
			s.Duplicate++
		}
	}
	return s
}

// order ranks the kinds of change the way a reader checks them.
var order = map[Kind]int{Duplicate: 0, Removed: 1, Added: 2, Changed: 3, Unchanged: 4}

// rank drops unchanged rows unless asked for, orders the rest the way a reader checks them
// (duplicates, then removed, added and changed rows, each biggest movement first, then by key)
// and applies the limit.
func rank(changes []Change, opts Options) []Change {
	if !opts.IncludeUnchanged {
		changes = slices.DeleteFunc(changes, func(ch Change) bool { return ch.Kind == Unchanged })
	}
	slices.SortStableFunc(changes, func(a, b Change) int {
		if by := cmp.Compare(order[a.Kind], order[b.Kind]); by != 0 {
			return by
		}
		if by := compareMagnitude(b.magnitude, a.magnitude); by != 0 {
			return by
		}
		return slices.Compare(a.Key, b.Key)
	})
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit > 0 && len(changes) > limit {
		changes = changes[:limit]
	}
	if changes == nil {
		changes = []Change{}
	}
	return changes
}

// compareMagnitude orders two magnitudes, a missing one (a duplicate's) first.
func compareMagnitude(a, b pgtype.Numeric) int {
	if !a.Valid || !b.Valid {
		return cmp.Compare(boolInt(a.Valid), boolInt(b.Valid))
	}
	c, _ := value.CompareNumeric(a, b)
	return c
}

// boolInt is 1 for true and 0 for false.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
