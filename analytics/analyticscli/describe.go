package analyticscli

import (
	"context"
	"fmt"
	"strings"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/rollup"
	"github.com/emoss08/keel/schema"
)

// Describe returns a compact description of the catalog for a language model's context: the
// catalog's own description (View.Describe without a policy: every entity, field and edge, with
// types, aggregations and enum values) followed by the rollups that answer reports on each entity
// fast. A caller's policy narrows the catalog the engine describes at run time.
func Describe(cat *analytics.Catalog, rollups ...*rollup.Rollup) string {
	var b strings.Builder
	b.WriteString(cat.For(context.Background(), nil).Describe())
	var lines []string
	for _, r := range rollups {
		e, ok := cat.EntityOf(r.Source())
		if !ok {
			continue
		}
		lines = append(lines, describeRollup(e, r))
	}
	if len(lines) > 0 {
		b.WriteString("rollups (reports within their dimensions, at their grain or coarser, run fast):\n")
		b.WriteString(strings.Join(lines, "\n") + "\n")
	}
	return b.String()
}

// describeRollup writes one rollup's line: its entity, grain, zone, dimensions and measures by
// field key.
func describeRollup(e *analytics.Entity, r *rollup.Rollup) string {
	key := func(c *schema.Column) string {
		idx := e.Table.Column(c.Name)
		for _, f := range e.Fields {
			if f.Column == idx {
				return f.Key
			}
		}
		return c.Name
	}
	var dims, aggs []string
	for _, d := range r.Dimensions() {
		dims = append(dims, key(d))
	}
	for _, a := range r.Aggregates() {
		arg := "rows"
		if a.Column != nil {
			arg = key(a.Column)
		}
		aggs = append(aggs, string(a.Agg)+" "+arg)
	}
	line := fmt.Sprintf("  %s on %s: by %s of %s in %s", r.Name(), e.Key, r.Unit(), key(r.GrainColumn()), r.Location())
	if len(dims) > 0 {
		line += " and " + strings.Join(dims, ", ")
	}
	return line + "; " + strings.Join(aggs, ", ")
}
