package analytics

import (
	"github.com/emoss08/keel"
)

// discovery builds the statement that finds a dynamic pivot's values: the pivot field's values
// as text, ranked by the ranking measure over the report's filtered rows, the first N.
//
// The report statement's columns depend on these values (one column per value), and its values
// are bound as parameters so its plan is shared across runs, so the runner sends the two
// statements in two round trips in one REPEATABLE READ transaction: both see the same snapshot,
// and the first is small (one grouped scan, N rows). A single statement would have to return
// positional columns with the values repeated on every row, and could not be prepared once per
// report shape.
func (c *compiler) discovery() (*keel.DynQuery, error) {
	t := c.def.Pivot.Top
	n, ok := countOf(t.N, c.params)
	if !ok {
		return nil, fail(CodeParameter, "/pivot/top/n", "the number of pivot values has no value")
	}
	by := c.byID[t.By]
	if by.outer {
		return nil, fail(CodeNotAllowed, "/pivot/top/by", "pivot values are ranked by a measure over the report's rows, and %s is computed over the grouped result", t.By)
	}
	q := c.source()
	f := newFrame(c, q)
	if err := c.where(f); err != nil {
		return nil, err
	}
	e, fp, err := f.field(c.def.Pivot.Field)
	if err != nil {
		return nil, err
	}
	m, err := f.column(by, "")
	if err != nil {
		return nil, err
	}
	v := display(e, fp).Cast(keel.TypeString)
	q.Select(v.As("value"), m.As("by")).Where(e.IsNotNull()).Group(e).
		Order(m.Desc().NullsLast(), v.Asc()).Limit(n)
	return q, q.Err()
}

// pivotValues reads the values a discovery statement found.
func pivotValues(res *keel.DynResult) []string {
	if res == nil || len(res.Columns) == 0 {
		return []string{}
	}
	out := make([]string, 0, res.Rows)
	col := &res.Columns[0]
	for i := range res.Rows {
		if !col.IsNull(i) {
			out = append(out, col.String[i])
		}
	}
	return out
}
