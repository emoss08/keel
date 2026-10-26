package rollup

import (
	"fmt"
	"slices"

	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// bucketColumn names the rollup column holding each grain bucket's start.
const bucketColumn = "bucket"

// rowCountColumn names the rollup column counting source rows.
const rowCountColumn = "row_count"

// colKind is the role of a rollup table column.
type colKind uint8

const (
	colTenant colKind = iota + 1
	colBucket
	colDim
	colAgg
)

// stored is an aggregate a rollup table stores: count (of rows, or of a column's non-null
// values), sum, min, max or distinct count.
type stored struct {
	fn  string
	col int32
}

// column is one column of the rollup table: its name, role, the source column it comes from (-1
// for bucket and the row count) and, for an aggregate, what it stores.
type column struct {
	name   string
	kind   colKind
	source int32
	agg    stored
}

// derive lays out the rollup table's columns and builds its schema.
func (r *Rollup) derive() error {
	for _, ci := range r.source.Tenant {
		r.cols = append(r.cols, column{name: r.source.Columns[ci].Name, kind: colTenant, source: ci})
	}
	r.cols = append(r.cols, column{name: bucketColumn, kind: colBucket, source: -1})
	for _, d := range r.dims {
		r.cols = append(r.cols, column{name: r.source.Columns[d].Name, kind: colDim, source: d})
	}
	for _, s := range r.storedAggs() {
		r.cols = append(r.cols, column{name: r.aggName(s), kind: colAgg, source: s.col, agg: s})
	}
	seen := map[string]bool{}
	for _, c := range r.cols {
		if seen[c.name] {
			return fmt.Errorf("the rollup table would have two columns named %s; rename one", c.name)
		}
		seen[c.name] = true
	}
	r.table = r.schemaTable()
	return nil
}

// storedAggs expands the measures into the aggregates the table stores, each once: an average
// becomes a sum and a count, the count of a column that is never null the row count.
func (r *Rollup) storedAggs() []stored {
	var out []stored
	add := func(s stored) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, m := range r.measures {
		switch m.agg {
		case ir.AggCount:
			add(r.countOf(m.col))
		case ir.AggSum:
			add(stored{fn: "sum", col: m.col})
		case ir.AggAvg:
			add(stored{fn: "sum", col: m.col})
			add(r.countOf(m.col))
		case ir.AggMin:
			add(stored{fn: "min", col: m.col})
		case ir.AggMax:
			add(stored{fn: "max", col: m.col})
		case ir.AggCountDistinct:
			add(stored{fn: "count_distinct", col: m.col})
		}
	}
	return out
}

// countOf returns the stored count of column col's values: the row count when col is -1 or never
// null.
func (r *Rollup) countOf(col int32) stored {
	if col < 0 || !r.source.Columns[col].MayBeNull() {
		return stored{fn: "count", col: -1}
	}
	return stored{fn: "count", col: col}
}

// aggName names the column of a stored aggregate: row_count, or the source column's name with
// _count, _sum, _min, _max or _distinct.
func (r *Rollup) aggName(s stored) string {
	if s.col < 0 {
		return rowCountColumn
	}
	suffix := map[string]string{"count": "_count", "sum": "_sum", "min": "_min", "max": "_max", "count_distinct": "_distinct"}
	return r.source.Columns[s.col].Name + suffix[s.fn]
}

// hasStored reports whether the table stores s, and returns its column index.
func (r *Rollup) hasStored(s stored) (int32, bool) {
	for i, c := range r.cols {
		if c.kind == colAgg && c.agg == s {
			return int32(i), true
		}
	}
	return -1, false
}

// colIndex returns the rollup column holding source column src as a tenant column or dimension.
func (r *Rollup) colIndex(src int32) (int32, bool) {
	for i, c := range r.cols {
		if (c.kind == colTenant || c.kind == colDim) && c.source == src {
			return int32(i), true
		}
	}
	return -1, false
}

// schemaTable builds the schema of the rollup table.
func (r *Rollup) schemaTable() *schema.Table {
	t := &schema.Table{
		Name:   r.name,
		Model:  "rollup." + r.name,
		Label:  r.source.Label + " Rollup",
		Plural: r.source.Label + " Rollups",
		Doc:    fmt.Sprintf("%s is the %s rollup of %s.", r.name, r.unit, r.source.Name),
	}
	for _, c := range r.cols {
		t.Columns = append(t.Columns, r.schemaColumn(c))
	}
	t.Relations = r.relations()
	schema.Derive(t)
	return t
}

// schemaColumn describes rollup column c.
func (r *Rollup) schemaColumn(c column) schema.Column {
	switch c.kind {
	case colTenant, colDim:
		sc := r.source.Columns[c.source]
		keep := schema.FlagNullable | schema.FlagJSONB | schema.FlagTenant
		if sc.Flags.Has(schema.FlagNullZero) {
			sc.Flags |= schema.FlagNullable
		}
		sc.Flags &= keep
		sc.Quoted = ""
		return sc
	case colBucket:
		return schema.Column{
			Name: bucketColumn, Field: "Bucket", Kind: schema.KindTime, Label: "Bucket",
			Flags:  r.nullableBucket(),
			Report: &schema.ReportInfo{Included: true, Kind: schema.Dimension, Type: schema.ValueTime, Aggs: schema.AggsOrdered},
		}
	}
	return r.aggColumn(c)
}

// nullableBucket flags the bucket nullable when the grain column is.
func (r *Rollup) nullableBucket() schema.ColFlag {
	if r.source.Columns[r.grain].MayBeNull() {
		return schema.FlagNullable
	}
	return 0
}

// aggColumn describes the column of a stored aggregate with the type Postgres gives it: counts
// are bigint, sum widens smallint and integer to bigint and bigint to numeric, and min and max
// keep the source type.
func (r *Rollup) aggColumn(c column) schema.Column {
	sc := schema.Column{Name: c.name, Field: pascal(c.name), Label: label(c.name)}
	switch c.agg.fn {
	case "count", "count_distinct":
		sc.Kind = schema.KindInt64
		sc.Report = &schema.ReportInfo{Kind: schema.Measure, Type: schema.ValueInt, Aggs: schema.AggsNumeric}
		return sc
	case "sum":
		src := r.source.Columns[c.source]
		sc.Flags = schema.FlagNullable
		sc.Kind, sc.Report = sumKind(src)
		return sc
	}
	src := r.source.Columns[c.source]
	sc.Kind, sc.Flags, sc.Report = src.Kind, schema.FlagNullable, src.Report
	return sc
}

// sumKind returns the kind and report facts of sum over src.
func sumKind(src schema.Column) (schema.Kind, *schema.ReportInfo) {
	info := func(t schema.ValueType) *schema.ReportInfo {
		return &schema.ReportInfo{Kind: schema.Measure, Type: t, Aggs: schema.AggsNumeric}
	}
	switch src.Kind {
	case schema.KindInt16, schema.KindInt32:
		return schema.KindInt64, info(schema.ValueInt)
	case schema.KindFloat32, schema.KindFloat64:
		return src.Kind, info(schema.ValueFloat)
	}
	return schema.KindOther, info(schema.ValueDecimal)
}

// relations carries over the source's belongs-to relations whose local key columns the rollup
// keeps, as tenant columns or dimensions.
func (r *Rollup) relations() []schema.Relation {
	var out []schema.Relation
	for _, rel := range r.source.Relations {
		if rel.Kind != schema.BelongsTo {
			continue
		}
		local := make([]int32, 0, len(rel.Local))
		for _, l := range rel.Local {
			if idx, ok := r.colIndex(l); ok {
				local = append(local, idx)
			}
		}
		if len(local) != len(rel.Local) {
			continue
		}
		rel.Local = local
		rel.Path, rel.Order = nil, nil
		out = append(out, rel)
	}
	return out
}
