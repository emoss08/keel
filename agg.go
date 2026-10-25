package keel

import (
	"slices"
	"unsafe"

	"github.com/emoss08/keel/schema"
)

// Agg is a computed value: an aggregate (count, sum, min, max, avg), a window function or a scalar
// function keel knows. It compares to values, orders, and filters or windows when it aggregates.
type Agg struct {
	fn       string
	arg      Expr
	ops      []any
	distinct bool
	star     bool
	custom   func(r *render)
	order    []Order
	within   []Order
	filter   Pred
	over     *Window
}

// Count is count(*).
func Count() Agg { return Agg{fn: "count", star: true} }

// Sum is sum(e).
func Sum(e Expr) Agg { return Agg{fn: "sum", arg: e} }

// Avg is avg(e).
func Avg(e Expr) Agg { return Agg{fn: "avg", arg: e} }

// Min is min(e).
func Min(e Expr) Agg { return Agg{fn: "min", arg: e} }

// Max is max(e).
func Max(e Expr) Agg { return Agg{fn: "max", arg: e} }

// CountOf is count(e): the rows where e is not NULL.
func CountOf(e Expr) Agg { return Agg{fn: "count", arg: e} }

// RowNumber is row_number(): the row's position in its window partition, from 1. Give it a window
// with Over or OverWindow.
func RowNumber() Agg { return Agg{fn: "row_number", star: true} }

// Rank is rank(): the row's rank in its partition by the window's order, with gaps after ties.
func Rank() Agg { return Agg{fn: "rank", star: true} }

// DenseRank is dense_rank(): rank without gaps.
func DenseRank() Agg { return Agg{fn: "dense_rank", star: true} }

// PercentRank is percent_rank(): (rank - 1) / (partition rows - 1), from 0 to 1.
func PercentRank() Agg { return Agg{fn: "percent_rank", star: true} }

// CumeDist is cume_dist(): the fraction of partition rows ordered before or tied with the row.
func CumeDist() Agg { return Agg{fn: "cume_dist", star: true} }

// Ntile is ntile(n): the bucket, from 1 to n, of the row when the partition is split into n
// buckets as evenly as possible. n is bound as a parameter.
func Ntile(n int) Agg { return Agg{fn: "ntile", ops: []any{int64(n)}} }

// Lag is lag(e, offset, def): e in the row offset rows before the current row in its partition,
// or def (a value bound as a parameter or an expression) when there is no such row. A nil def is
// NULL.
func Lag(e Expr, offset int, def any) Agg { return offsetAgg("lag", e, offset, def) }

// Lead is lead(e, offset, def): e in the row offset rows after the current row.
func Lead(e Expr, offset int, def any) Agg { return offsetAgg("lead", e, offset, def) }

func offsetAgg(fn string, e Expr, offset int, def any) Agg {
	a := Agg{fn: fn, arg: e, ops: []any{int64(offset)}}
	if def != nil {
		a.ops = append(a.ops, def)
	}
	return a
}

// FirstValue is first_value(e): e in the first row of the window frame.
func FirstValue(e Expr) Agg { return Agg{fn: "first_value", arg: e} }

// LastValue is last_value(e): e in the last row of the window frame. The default frame ends at
// the current row's last peer; give the window a frame that reaches UnboundedFollowing to read
// the partition's last row.
func LastValue(e Expr) Agg { return Agg{fn: "last_value", arg: e} }

// NthValue is nth_value(e, n): e in the n-th row of the window frame, from 1.
func NthValue(e Expr, n int) Agg { return Agg{fn: "nth_value", arg: e, ops: []any{int64(n)}} }

// ArrayAgg is array_agg(e). OrderBy orders the elements.
func ArrayAgg(e Expr) Agg { return Agg{fn: "array_agg", arg: e} }

// StringAgg is string_agg(e, sep). OrderBy orders the pieces.
func StringAgg(e Expr, sep string) Agg { return Agg{fn: "string_agg", arg: e, ops: []any{sep}} }

// PercentileCont is percentile_cont(fraction) WITHIN GROUP (ORDER BY order): the value at
// fraction (0 to 1) of the ordered group, interpolated between rows.
func PercentileCont(fraction float64, order Order) Agg {
	return Agg{fn: "percentile_cont", ops: []any{fraction}, within: []Order{order}}
}

// PercentileDisc is percentile_disc(fraction) WITHIN GROUP (ORDER BY order): the first value of
// the ordered group whose position reaches fraction.
func PercentileDisc(fraction float64, order Order) Agg {
	return Agg{fn: "percentile_disc", ops: []any{fraction}, within: []Order{order}}
}

// Mode is mode() WITHIN GROUP (ORDER BY order): the group's most frequent value, the first in
// order on a tie.
func Mode(order Order) Agg { return Agg{fn: "mode", star: true, within: []Order{order}} }

// Grouping is GROUPING(es...): a bit mask with a 1 for each of es that the row's grouping set
// does not group by. In a query with GroupingSets, Rollup or Cube it tells a subtotal or total row
// from a detail row; it may appear in Pick, Having and Order. Each of es must be written exactly
// as it is in the grouping.
func Grouping(es ...Expr) Agg {
	return Agg{fn: "GROUPING", custom: func(r *render) {
		if len(es) == 0 {
			r.fail(errf("Grouping needs at least one expression"))
			return
		}
		r.s("GROUPING(")
		writeExprs(r, es)
		r.b(')')
	}}
}

// OrderBy orders the rows an aggregate reads, for an aggregate whose result depends on their order:
// array_agg(e ORDER BY …), string_agg(e, sep ORDER BY …).
func (a Agg) OrderBy(os ...Order) Agg { a.order = slices.Clip(os); return a }

// Distinct aggregates the distinct values of the argument: array_agg(DISTINCT e).
func (a Agg) Distinct() Agg { a.distinct = true; return a }

// Filter restricts the rows an aggregate sees: count(*) FILTER (WHERE p).
func (a Agg) Filter(p Pred) Agg { a.filter = p; return a }

// Over turns the aggregate into a window function over the rows that share partition, ordered
// by order. It is OverWindow(PartitionBy(partition...).OrderBy(order...)).
func (a Agg) Over(partition []Expr, order ...Order) Agg {
	return a.OverWindow(Window{partition: partition, order: order})
}

// OverWindow turns the aggregate into a window function over w: a running or moving aggregate, a
// rank, an offset or a value in the window frame.
func (a Agg) OverWindow(w Window) Agg {
	a.over = &w
	return a
}

func (a Agg) writeSQL(r *render) {
	if a.custom != nil {
		a.custom(r)
		return
	}
	r.s(a.fn)
	r.b('(')
	switch {
	case a.star && a.fn == "count":
		r.b('*')
	case a.star:
	case a.arg == nil:
	default:
		if a.distinct {
			r.s("DISTINCT ")
		}
		a.arg.writeSQL(r)
	}
	for i := range a.ops {
		if a.arg != nil || i > 0 {
			r.s(", ")
		}
		writeOperand(r, a.ops[i], unsafe.Pointer(&a.ops[i]), a.fn)
	}
	if len(a.order) > 0 {
		r.s(" ORDER BY ")
		writeOrders(r, a.order)
	}
	r.b(')')
	if len(a.within) > 0 {
		r.s(" WITHIN GROUP (ORDER BY ")
		writeOrders(r, a.within)
		r.b(')')
	}
	if a.filter != nil {
		r.s(" FILTER (WHERE ")
		a.filter.writeSQL(r)
		r.b(')')
	}
	if a.over != nil {
		r.s(" OVER ")
		a.over.writeOver(r)
	}
}

// Eq is a = v.
func (a Agg) Eq(v any) Pred { return cmp(a, " = ", v) }

// Ne is a <> v.
func (a Agg) Ne(v any) Pred { return cmp(a, " <> ", v) }

// Gt is a > v.
func (a Agg) Gt(v any) Pred { return cmp(a, " > ", v) }

// Gte is a >= v.
func (a Agg) Gte(v any) Pred { return cmp(a, " >= ", v) }

// Lt is a < v.
func (a Agg) Lt(v any) Pred { return cmp(a, " < ", v) }

// Lte is a <= v.
func (a Agg) Lte(v any) Pred { return cmp(a, " <= ", v) }

// Asc orders by a ascending.
func (a Agg) Asc() Order { return Order{e: a} }

// Desc orders by a descending.
func (a Agg) Desc() Order { return Order{e: a, desc: true} }

// Window is a window specification: the partition, the order and the frame of a window function.
// The zero Window is OVER (): every row of the result, unordered. Build one with PartitionBy,
// OrderBy or NamedWindow and give it to Agg.OverWindow; methods return a changed copy.
//
//	running := keel.Sum(s.Weight).OverWindow(keel.PartitionBy(s.CustomerID).
//	    OrderBy(s.CreatedAt.Asc()).Rows(keel.UnboundedPreceding, keel.CurrentRow))
type Window struct {
	name      string
	partition []Expr
	order     []Order
	frame     string
	start     FrameBound
	end       FrameBound
	exclude   FrameExclusion
}

// FrameBound is one end of a window frame: UnboundedPreceding, Preceding(n), CurrentRow,
// Following(n) or UnboundedFollowing.
type FrameBound struct {
	kind   uint8
	offset any
}

const (
	boundNone uint8 = iota
	boundUnboundedPreceding
	boundPreceding
	boundCurrentRow
	boundFollowing
	boundUnboundedFollowing
)

var (
	// UnboundedPreceding is the first row of the partition.
	UnboundedPreceding = FrameBound{kind: boundUnboundedPreceding}
	// CurrentRow is the current row (in Range and Groups frames, its first or last peer).
	CurrentRow = FrameBound{kind: boundCurrentRow}
	// UnboundedFollowing is the last row of the partition.
	UnboundedFollowing = FrameBound{kind: boundUnboundedFollowing}
)

// Preceding is the frame bound offset before the current row: a row count in a Rows frame, a peer
// group count in a Groups frame, and a distance in the order column's type in a Range frame (an
// integer, a float, a decimal, or a time.Duration for a timestamp order). offset is bound as a
// parameter.
func Preceding(offset any) FrameBound { return FrameBound{kind: boundPreceding, offset: offset} }

// Following is the frame bound offset after the current row, measured as Preceding measures it.
func Following(offset any) FrameBound { return FrameBound{kind: boundFollowing, offset: offset} }

// FrameExclusion removes rows around the current row from a window frame.
type FrameExclusion string

const (
	// ExcludeNoOthers excludes nothing, the default.
	ExcludeNoOthers FrameExclusion = "NO OTHERS"
	// ExcludeCurrentRow excludes the current row.
	ExcludeCurrentRow FrameExclusion = "CURRENT ROW"
	// ExcludeGroup excludes the current row and its peers.
	ExcludeGroup FrameExclusion = "GROUP"
	// ExcludeTies excludes the current row's peers but not the row itself.
	ExcludeTies FrameExclusion = "TIES"
)

// PartitionBy starts a window whose partitions are the rows that share es.
func PartitionBy(es ...Expr) Window { return Window{partition: es} }

// OrderBy starts a window over every row, ordered by os.
func OrderBy(os ...Order) Window { return Window{order: os} }

// NamedWindow refers to a window the query declares with Window. It may add an order, when the
// named window has none, and a frame; it cannot add a partition.
func NamedWindow(name string) Window { return Window{name: name} }

// PartitionBy adds partition expressions.
func (w Window) PartitionBy(es ...Expr) Window {
	w.partition = append(slices.Clip(w.partition), es...)
	return w
}

// OrderBy adds order terms.
func (w Window) OrderBy(os ...Order) Window {
	w.order = append(slices.Clip(w.order), os...)
	return w
}

// Rows sets the frame to ROWS BETWEEN start AND end: offsets count rows.
func (w Window) Rows(start, end FrameBound) Window { return w.withFrame("ROWS", start, end) }

// Range sets the frame to RANGE BETWEEN start AND end: offsets are distances in the value of the
// window's single order expression, and CurrentRow includes the row's peers.
func (w Window) Range(start, end FrameBound) Window { return w.withFrame("RANGE", start, end) }

// Groups sets the frame to GROUPS BETWEEN start AND end: offsets count peer groups.
func (w Window) Groups(start, end FrameBound) Window { return w.withFrame("GROUPS", start, end) }

// Exclude removes rows around the current row from the frame. It needs a frame.
func (w Window) Exclude(x FrameExclusion) Window {
	w.exclude = x
	return w
}

func (w Window) withFrame(kind string, start, end FrameBound) Window {
	w.frame, w.start, w.end = kind, start, end
	return w
}

// writeOver writes the window after OVER: its name alone, or its specification in parentheses.
func (w *Window) writeOver(r *render) {
	if w.name != "" && len(w.partition) == 0 && len(w.order) == 0 && w.frame == "" {
		if !validAlias(w.name) {
			r.fail(errf("window name %q must match [a-z_][a-z0-9_]*", w.name))
			return
		}
		r.s(schema.Quote(w.name))
		return
	}
	r.b('(')
	w.writeSpec(r)
	r.b(')')
}

// writeSpec writes the window specification without parentheses.
func (w *Window) writeSpec(r *render) {
	sep := false
	space := func() {
		if sep {
			r.b(' ')
		}
		sep = true
	}
	if w.name != "" {
		if !validAlias(w.name) {
			r.fail(errf("window name %q must match [a-z_][a-z0-9_]*", w.name))
			return
		}
		if len(w.partition) > 0 {
			r.fail(errf("window %s: a window that refers to a named window cannot add a partition",
				w.name))
			return
		}
		space()
		r.s(schema.Quote(w.name))
	}
	if len(w.partition) > 0 {
		space()
		r.s("PARTITION BY ")
		writeExprs(r, w.partition)
	}
	if len(w.order) > 0 {
		space()
		r.s("ORDER BY ")
		writeOrders(r, w.order)
	}
	if w.frame == "" {
		if w.exclude != "" {
			r.fail(errf("a window exclusion needs a frame"))
		}
		return
	}
	space()
	r.s(w.frame)
	r.s(" BETWEEN ")
	w.start.write(r, w.frame)
	r.s(" AND ")
	w.end.write(r, w.frame)
	if w.exclude != "" {
		r.s(" EXCLUDE ")
		r.s(string(w.exclude))
	}
}

func (b *FrameBound) write(r *render, frame string) {
	switch b.kind {
	case boundUnboundedPreceding:
		r.s("UNBOUNDED PRECEDING")
	case boundCurrentRow:
		r.s("CURRENT ROW")
	case boundUnboundedFollowing:
		r.s("UNBOUNDED FOLLOWING")
	case boundPreceding, boundFollowing:
		if b.offset == nil {
			r.fail(errf("%s frame offset is nil", frame))
			return
		}
		writeOperand(r, b.offset, unsafe.Pointer(&b.offset), frame)
		if b.kind == boundPreceding {
			r.s(" PRECEDING")
		} else {
			r.s(" FOLLOWING")
		}
	default:
		r.fail(errf("%s frame needs a start and an end bound", frame))
	}
}

// namedWindow is one WINDOW clause entry of a query.
type namedWindow struct {
	name string
	w    Window
}

// writeWindows writes WINDOW name AS (spec), … for ws.
func writeWindows(r *render, ws []namedWindow) {
	for i := range ws {
		if i > 0 {
			r.s(", ")
		}
		if !validAlias(ws[i].name) {
			r.fail(errf("window name %q must match [a-z_][a-z0-9_]*", ws[i].name))
			return
		}
		r.s(schema.Quote(ws[i].name))
		r.s(" AS (")
		ws[i].w.writeSpec(r)
		r.b(')')
	}
}

// groupingElement is a GROUP BY element other than a plain expression: GROUPING SETS, ROLLUP or
// CUBE. It is listed with the query's other GROUP BY expressions.
type groupingElement struct {
	kind string
	sets [][]Expr
}

func (g groupingElement) writeSQL(r *render) {
	if len(g.sets) == 0 {
		r.fail(errf("%s needs at least one expression", g.kind))
		return
	}
	r.s(g.kind)
	r.s(" (")
	for i, set := range g.sets {
		if i > 0 {
			r.s(", ")
		}
		if g.kind == "GROUPING SETS" {
			r.b('(')
			writeExprs(r, set)
			r.b(')')
			continue
		}
		set[0].writeSQL(r)
	}
	r.b(')')
}

func groupingSets(sets [][]Expr) Expr { return groupingElement{kind: "GROUPING SETS", sets: sets} }

func rollup(es []Expr) Expr { return groupingElement{kind: "ROLLUP", sets: singletons(es)} }

func cube(es []Expr) Expr { return groupingElement{kind: "CUBE", sets: singletons(es)} }

func singletons(es []Expr) [][]Expr {
	out := make([][]Expr, len(es))
	for i, e := range es {
		out[i] = []Expr{e}
	}
	return out
}

// writeExprs writes es separated by commas.
func writeOrders(r *render, os []Order) {
	for i, o := range os {
		if i > 0 {
			r.s(", ")
		}
		o.writeSQL(r)
	}
}

func writeExprs(r *render, es []Expr) {
	for i, e := range es {
		if i > 0 {
			r.s(", ")
		}
		e.writeSQL(r)
	}
}
