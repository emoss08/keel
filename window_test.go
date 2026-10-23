package keel_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

func TestWindowAggregates(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	s := fleetdb.Shipments
	byPro := keel.OrderBy(s.ProNumber.Asc())
	type row struct {
		Pro     string
		Running float64
		Moving  float64
		Share   float64
		Rank    int64
		Change  *float64
		Next    *float64
		First   float64
		Last    float64
		Second  *float64
		Count   int64
	}
	rows := must(keel.Into[row](w.ctx, d, s.Pick(
		s.ProNumber,
		keel.Sum(s.Total).OverWindow(byPro.Rows(keel.UnboundedPreceding, keel.CurrentRow)),
		keel.Avg(s.Total).OverWindow(byPro.Rows(keel.Preceding(2), keel.CurrentRow)),
		keel.Raw("? * 100.0 / ?", s.Total, keel.Sum(s.Total).OverWindow(keel.Window{})),
		keel.Rank().OverWindow(keel.PartitionBy(s.CustomerID).OrderBy(s.Total.Desc())),
		keel.Raw("? - ?", s.Total, keel.Lag(s.Total, 1, nil).OverWindow(byPro)),
		keel.Lead(s.Total, 1, nil).OverWindow(byPro),
		keel.FirstValue(s.Total).OverWindow(byPro),
		keel.LastValue(s.Total).OverWindow(byPro.Rows(keel.CurrentRow, keel.UnboundedFollowing)),
		keel.NthValue(s.Total, 2).OverWindow(byPro),
		keel.Count().Filter(s.Hazmat.IsTrue()).OverWindow(byPro),
	).Order(s.ProNumber.Asc())))(t)
	eq(t, len(rows), 20)
	totalSum := 0.0
	for i := range 20 {
		totalSum += 100 + float64(i)
	}
	hazmat := int64(0)
	for i, r := range rows {
		v := 100 + float64(i)
		eq(t, r.Running, 100*float64(i+1)+float64(i*(i+1))/2)
		switch i {
		case 0:
			eq(t, r.Moving, 100.0)
			if r.Change != nil || r.Second != nil {
				t.Fatalf("first row: %+v", r)
			}
		case 1:
			eq(t, r.Moving, 100.5)
		default:
			eq(t, r.Moving, v-1)
			eq(t, *r.Change, 1.0)
		}
		if i > 0 {
			eq(t, *r.Second, 101.0)
		}
		if i < 19 {
			eq(t, *r.Next, v+1)
		} else if r.Next != nil {
			t.Fatal("lead past the end is not NULL")
		}
		if math.Abs(r.Share-v*100/totalSum) > 1e-9 {
			t.Fatalf("share %v", r.Share)
		}
		wantRank := int64(7 - i/3)
		if i%3 == 2 {
			wantRank = int64(6 - i/3)
		}
		eq(t, r.Rank, wantRank)
		eq(t, r.First, 100.0)
		eq(t, r.Last, 119.0)
		if i%5 == 0 {
			hazmat++
		}
		eq(t, r.Count, hazmat)
	}
}

func TestWindowRanksAndFrames(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	s := fleetdb.Shipments
	byPriority := keel.OrderBy(s.Priority.Asc())
	type row struct {
		Priority  int16
		Row       int64
		Dense     int64
		Percent   float64
		Cume      float64
		Tile      int32
		RangeN    int64
		GroupsN   int64
		ExGroup   int64
		ExCurrent int64
		ExTies    int64
		ExNone    int64
	}
	everything := byPriority.Range(keel.UnboundedPreceding, keel.UnboundedFollowing)
	rows := must(keel.Into[row](w.ctx, d, s.Pick(
		s.Priority,
		keel.RowNumber().OverWindow(byPriority.OrderBy(s.ProNumber.Asc())),
		keel.DenseRank().OverWindow(byPriority),
		keel.PercentRank().OverWindow(byPriority),
		keel.CumeDist().OverWindow(byPriority),
		keel.Ntile(4).OverWindow(byPriority.OrderBy(s.ProNumber.Asc())),
		keel.Count().OverWindow(byPriority.Range(keel.Preceding(1), keel.CurrentRow)),
		keel.Count().OverWindow(byPriority.Groups(keel.CurrentRow, keel.Following(1))),
		keel.Count().OverWindow(everything.Exclude(keel.ExcludeGroup)),
		keel.Count().OverWindow(everything.Exclude(keel.ExcludeCurrentRow)),
		keel.Count().OverWindow(everything.Exclude(keel.ExcludeTies)),
		keel.Count().OverWindow(everything.Exclude(keel.ExcludeNoOthers)),
	).Order(s.Priority.Asc(), s.ProNumber.Asc())))(t)
	eq(t, len(rows), 20)
	size := map[int16]int64{0: 7, 1: 7, 2: 6}
	before := map[int16]int64{0: 0, 1: 7, 2: 14}
	for i, r := range rows {
		p := r.Priority
		eq(t, r.Row, int64(i+1))
		eq(t, r.Dense, int64(p+1))
		eq(t, r.Percent, float64(before[p])/19)
		eq(t, r.Cume, float64(before[p]+size[p])/20)
		eq(t, r.Tile, int32(i/5+1))
		eq(t, r.RangeN, map[int16]int64{0: 7, 1: 14, 2: 13}[p])
		eq(t, r.GroupsN, map[int16]int64{0: 14, 1: 13, 2: 6}[p])
		eq(t, r.ExGroup, 20-size[p])
		eq(t, r.ExCurrent, int64(19))
		eq(t, r.ExTies, 20-size[p]+1)
		eq(t, r.ExNone, int64(20))
	}
}

func TestWindowRangeOverTime(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	wk := fleetdb.Workers
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		ok(t, wk.Create(ctx, d, &fleet.Worker{FirstName: "W", LastName: string(rune('a' + i))}))
		_, err := wk.Where(wk.LastName.Eq(string(rune('a'+i)))).Update(ctx, d,
			wk.HiredAt.To(base.Add(time.Duration(i)*24*time.Hour)))
		ok(t, err)
	}
	type row struct {
		Last string
		N    int64
	}
	rows := must(keel.Into[row](ctx, d, wk.Pick(wk.LastName,
		keel.Count().OverWindow(keel.OrderBy(wk.HiredAt.Asc()).
			Range(keel.Preceding(36*time.Hour), keel.CurrentRow))).
		Order(wk.HiredAt.Asc())))(t)
	eq(t, len(rows), 5)
	eq(t, rows[0].N, int64(1))
	for _, r := range rows[1:] {
		eq(t, r.N, int64(2))
	}
}

func TestNamedWindowsAndGroupedWindows(t *testing.T) {
	d := db(t)
	w := seed(t, 20)
	s := fleetdb.Shipments
	type row struct {
		Pro     string
		N       int64
		Running float64
	}
	q := s.Pick(s.ProNumber,
		keel.RowNumber().OverWindow(keel.NamedWindow("per_customer")),
		keel.Sum(s.Total).OverWindow(keel.NamedWindow("per_customer").
			Rows(keel.UnboundedPreceding, keel.CurrentRow))).
		Window("per_customer", keel.PartitionBy(s.CustomerID).OrderBy(s.ProNumber.Asc())).
		Order(s.ProNumber.Asc())
	sql, _, err := q.ToSQL(w.ctx, d)
	ok(t, err)
	if !strings.Contains(sql, `OVER "per_customer"`) ||
		!strings.Contains(sql, `OVER ("per_customer" ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)`) ||
		!strings.Contains(sql, ` WINDOW "per_customer" AS (PARTITION BY s."customer_id" ORDER BY s."pro_number" ASC) ORDER BY`) {
		t.Fatalf("named window:\n%s", sql)
	}
	rows := must(keel.Into[row](w.ctx, d, q))(t)
	eq(t, len(rows), 20)
	eq(t, rows[3], row{"PRO-0003", 2, 203})
	eq(t, rows[6].Running, 100.0+103+106)

	type share struct {
		Customer string
		Total    float64
		Percent  float64
		Rank     int64
	}
	code := s.Customer().Code
	shares := must(keel.Into[share](w.ctx, d, s.Pick(code, s.Total.Sum(),
		keel.Raw("round(? * 100 / ?, 4)", s.Total.Sum(), keel.Sum(s.Total.Sum()).OverWindow(keel.Window{})),
		keel.Rank().OverWindow(keel.OrderBy(s.Total.Sum().Desc()))).
		Group(code).Order(code.Asc())))(t)
	eq(t, len(shares), 3)
	eq(t, shares[0].Total, 763.0)
	eq(t, shares[0].Percent, math.Round(763.0*100/2190*10000)/10000)
	eq(t, shares[0].Rank, int64(2))

	_, _, err = s.Pick(keel.RowNumber().OverWindow(keel.NamedWindow("bad name"))).ToSQL(w.ctx, d)
	if err == nil {
		t.Fatal("an invalid window name rendered")
	}
	_, _, err = s.Pick(keel.RowNumber().OverWindow(keel.NamedWindow("w").PartitionBy(s.ID))).
		Window("w", keel.OrderBy(s.ID.Asc())).ToSQL(w.ctx, d)
	if err == nil || !strings.Contains(err.Error(), "cannot add a partition") {
		t.Fatalf("partition on a named window: %v", err)
	}
	_, _, err = s.Pick(keel.Count().OverWindow(keel.Window{}.Exclude(keel.ExcludeTies))).ToSQL(w.ctx, d)
	if err == nil || !strings.Contains(err.Error(), "needs a frame") {
		t.Fatalf("exclusion without a frame: %v", err)
	}
	_, _, err = s.Pick(keel.Count().OverWindow(keel.Window{}.Rows(keel.Preceding(nil), keel.CurrentRow))).
		ToSQL(w.ctx, d)
	if err == nil {
		t.Fatal("a nil frame offset rendered")
	}
}
