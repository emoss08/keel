package rollup

import (
	"strconv"
	"strings"

	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// Bookkeeping tables every rollup shares.
const (
	dirtyTable = "keel_rollup_dirty"
	stateTable = "keel_rollup_state"
)

// Sentinel buckets of keel_rollup_dirty: wholeTenant marks every bucket of a tenant, nullBucket the
// rows whose grain column is null.
const (
	wholeTenant = "-infinity"
	nullBucket  = "infinity"
)

// dirtyTableSQL creates keel_rollup_dirty. A row records that a transaction (xid) changed source
// rows in one slice of a rollup: a tenant and a grain bucket. Each transaction writes a slice at
// most once and never conflicts with another, so writers never wait on each other.
const dirtyTableSQL = `CREATE TABLE IF NOT EXISTS keel_rollup_dirty (
    rollup text COLLATE "C" NOT NULL,
    tenant_key text COLLATE "C" NOT NULL,
    bucket timestamptz NOT NULL,
    xid xid8 NOT NULL,
    marked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (rollup, tenant_key, bucket, xid)
);
CREATE INDEX IF NOT EXISTS keel_rollup_dirty_marked ON keel_rollup_dirty (marked_at);
CREATE TABLE IF NOT EXISTS keel_rollup_state (
    rollup text COLLATE "C" PRIMARY KEY,
    ready_at timestamptz NOT NULL
);`

// lit returns s as a SQL string literal.
func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// qcol returns alias.column for column name.
func qcol(alias, name string) string {
	if alias == "" {
		return schema.Quote(name)
	}
	return alias + "." + schema.Quote(name)
}

// interval is the length of one bucket of unit as a SQL interval literal.
func interval(unit ir.Bucket) string {
	switch unit {
	case ir.BucketHour:
		return "interval '1 hour'"
	case ir.BucketWeek:
		return "interval '7 days'"
	case ir.BucketMonth:
		return "interval '1 month'"
	case ir.BucketQuarter:
		return "interval '3 months'"
	case ir.BucketYear:
		return "interval '1 year'"
	}
	return "interval '1 day'"
}

// bucketExpr is the grain bucket of a source row under alias, as a timestamptz at the bucket's
// start in the rollup's zone.
func (r *Rollup) bucketExpr(alias string) string {
	c := qcol(alias, r.source.Columns[r.grain].Name)
	unit, zone := lit(string(r.unit)), lit(r.zoneName)
	switch r.grainType {
	case schema.ValueEpoch:
		return "date_trunc(" + unit + ", to_timestamp(" + c + "), " + zone + ")"
	case schema.ValueDate:
		return "(date_trunc(" + unit + ", " + c + "::timestamp) AT TIME ZONE " + zone + ")"
	}
	return "date_trunc(" + unit + ", " + c + ", " + zone + ")"
}

// upperExpr is the start of the bucket after bucket b, computed on the zone's wall clock so a
// day across a daylight saving change is still one day.
func (r *Rollup) upperExpr(b string) string {
	zone := lit(r.zoneName)
	return "((" + b + " AT TIME ZONE " + zone + ") + " + interval(r.unit) + ") AT TIME ZONE " + zone
}

// rangeExpr is true for the source rows under alias in the bucket starting at b, written on the
// grain column itself so an index on it serves the read.
func (r *Rollup) rangeExpr(alias, b string) string {
	c := qcol(alias, r.source.Columns[r.grain].Name)
	hi := r.upperExpr(b)
	switch r.grainType {
	case schema.ValueEpoch:
		return c + " >= extract(epoch FROM " + b + ")::int8 AND " + c + " < extract(epoch FROM " + hi + ")::int8"
	case schema.ValueDate:
		zone := lit(r.zoneName)
		return c + " >= (" + b + " AT TIME ZONE " + zone + ")::date AND " + c + " < ((" + hi + ") AT TIME ZONE " + zone + ")::date"
	}
	return c + " >= " + b + " AND " + c + " < " + hi
}

// tenantKeyExpr is the data-versions tenant key of a source row under alias (see
// keel.TenantKeyOf): the tenant columns as text, "\" and "/" escaped with "\", joined with "/".
func (r *Rollup) tenantKeyExpr(alias string) string {
	if len(r.source.Tenant) == 0 {
		return "''"
	}
	parts := make([]string, len(r.source.Tenant))
	for i, ci := range r.source.Tenant {
		c := qcol(alias, r.source.Columns[ci].Name)
		parts[i] = `replace(replace(coalesce(` + c + `::text, ''), '\', '\\'), '/', '\/')`
	}
	return strings.Join(parts, " || '/' || ")
}

// aggExpr is the SQL of rollup column c over source rows under alias.
func (r *Rollup) aggExpr(c column, alias string) string {
	switch c.kind {
	case colTenant, colDim:
		return qcol(alias, r.source.Columns[c.source].Name)
	case colBucket:
		return r.bucketExpr(alias)
	}
	if c.agg.col < 0 {
		return "count(*)"
	}
	arg := qcol(alias, r.source.Columns[c.agg.col].Name)
	if c.agg.fn == "count_distinct" {
		return "count(DISTINCT " + arg + ")"
	}
	return c.agg.fn + "(" + arg + ")"
}

// selectList is the rollup table's columns computed from source rows under alias, and the GROUP
// BY positions of its keys.
func (r *Rollup) selectList(alias string) (string, string) {
	var sel, group []string
	for i, c := range r.cols {
		if c.kind != colAgg {
			group = append(group, strconv.Itoa(i+1))
		}
		sel = append(sel, r.aggExpr(c, alias)+" AS "+schema.Quote(c.name))
	}
	return strings.Join(sel, ", "), strings.Join(group, ", ")
}

// keyColumns is the quoted list of the rollup table's key columns: tenant, bucket, dimensions.
func (r *Rollup) keyColumns() string {
	var out []string
	for _, c := range r.cols {
		if c.kind != colAgg {
			out = append(out, schema.Quote(c.name))
		}
	}
	return strings.Join(out, ", ")
}

// columnList is the quoted list of every rollup table column.
func (r *Rollup) columnList() string {
	out := make([]string, len(r.cols))
	for i, c := range r.cols {
		out[i] = schema.Quote(c.name)
	}
	return strings.Join(out, ", ")
}

// relevantColumns are the source columns a rollup reads: tenant, grain, dimensions and measured
// columns, each once, in table order.
func (r *Rollup) relevantColumns() []int32 {
	set := map[int32]bool{r.grain: true}
	for _, ci := range r.source.Tenant {
		set[ci] = true
	}
	for _, d := range r.dims {
		set[d] = true
	}
	for _, m := range r.measures {
		if m.col >= 0 {
			set[m.col] = true
		}
	}
	var out []int32
	for i := range r.source.Columns {
		if set[int32(i)] {
			out = append(out, int32(i))
		}
	}
	return out
}
