package keel_test

import (
	"strings"
	"testing"

	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestJoinsBeforeTheExplicitJoinsThatUseThem(t *testing.T) {
	d := db(t)
	w := seed(t, 6)
	m := fleetdb.Moves
	cx := fleetdb.Customers.As("cx")
	q := m.Pick(m.ID).Join(cx, cx.ID.EqCol(m.Shipment().Customer().ID)).Where(cx.Code.Eq("C1"))
	sql, _, err := q.ToSQL(w.ctx, d)
	ok(t, err)
	joined := strings.Index(sql, `LEFT JOIN "customers" c `)
	explicit := strings.Index(sql, `JOIN "customers" cx `)
	if joined < 0 || explicit < 0 || joined > explicit {
		t.Fatalf("a two-hop path an explicit join refers to is joined after it:\n%s", sql)
	}
	eq(t, must(q.Count(w.ctx, d))(t), int64(4))
}
