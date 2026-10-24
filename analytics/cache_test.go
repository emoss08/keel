package analytics_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/export"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
)

// memCache is an in-memory analytics.Cache that counts its hits and misses.
type memCache struct {
	mu           sync.Mutex
	entries      map[string][]byte
	expires      map[string]time.Time
	hits, misses int
}

func newMemCache() *memCache {
	return &memCache{entries: map[string][]byte{}, expires: map[string]time.Time{}}
}

func (c *memCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.entries[key]
	if !ok || time.Now().After(c.expires[key]) {
		c.misses++
		return nil, false, nil
	}
	c.hits++
	return b, true, nil
}

func (c *memCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = value
	c.expires[key] = time.Now().Add(ttl)
	return nil
}

func TestCacheHitsUntilTheDataChanges(t *testing.T) {
	w := seedWorld(t)
	cache := newMemCache()
	r := runner(t, analytics.RunnerOptions{Cache: cache})
	report := def(t, manyMeasures)
	req := analytics.Request{Definition: report}
	first, err := r.Preview(w.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Preview(w.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached() || !second.Cached() || cache.hits != 1 {
		t.Fatalf("cached %v %v, %d hits", first.Cached(), second.Cached(), cache.hits)
	}
	want(t, rows(t, second), rows(t, first.Reader())...)
	if second.Columns()[1].Display.Style != "currency" {
		t.Fatalf("the cached schema lost its display: %+v", second.Columns()[1])
	}
	for range 2 {
		if n, err := r.Count(w.ctx, req); err != nil || n != 3 {
			t.Fatalf("count %d %v", n, err)
		}
	}
	if cache.hits != 2 {
		t.Fatalf("%d hits after counting twice", cache.hits)
	}
	other := seedRows(t, fixture[:1])
	if res, err := r.Preview(other.ctx, req); err != nil || res.Cached() {
		t.Fatalf("another tenant read the cache: %v %v", res.Cached(), err)
	}
	ok(t, fleetdb.Moves.Create(w.ctx, db(t), &fleet.Move{ShipmentID: "shp_P05", Sequence: 1, Status: fleet.StatusDelivered}))
	ok(t, db(t).FlushDataVersions(context.Background()))
	third, err := r.Preview(w.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if third.Cached() {
		t.Fatal("a write to moves left the cache fresh")
	}
	if got := rows(t, third); got[1] != "Globex | 570.75 | 6 | 6 | 3 | 10 | 1.5 | 15 | 5" {
		t.Fatalf("rows after the write: %v", got)
	}
	page, err := r.Page(w.ctx, req, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Page(w.ctx, req, "", 2)
	if err != nil || !again.Cached() || again.Cursor() != page.Cursor() || page.Cursor() == "" {
		t.Fatalf("page cache: %v %v", again.Cached(), err)
	}
}

func TestRunStreamsAndExports(t *testing.T) {
	w := seedWorld(t)
	cache := newMemCache()
	r := runner(t, analytics.RunnerOptions{StreamBatch: 2, Cache: cache})
	report := def(t, `{"entity": "shipment", "columns": [
		{"id": "pro", "dimension": {"field": "proNumber"}},
		{"id": "customer", "dimension": {"field": "customer.name"}},
		{"id": "total", "dimension": {"field": "total"}},
		{"id": "created", "dimension": {"field": "createdAt"}}
	], "filters": {"field": "proNumber", "op": "starts_with", "value": "p0"}, "sort": [{"column": "pro"}]}`)
	stream, err := r.Run(w.ctx, analytics.Request{Definition: report})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	stats, err := export.CSV(w.ctx, &buf, stream, export.CSVOptions{Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Rows != 9 || len(records) != 10 || strings.Join(records[0], ",") != "pro,customer,total,created" ||
		strings.Join(records[3], ",") != "P03,Acme,300.0000,1769914800" {
		t.Fatalf("csv %d rows: %v", stats.Rows, records)
	}
	stream, err = r.Run(w.ctx, analytics.Request{Definition: report})
	if err != nil {
		t.Fatal(err)
	}
	var xlsx bytes.Buffer
	if _, err := export.XLSX(w.ctx, &xlsx, stream, export.Options{}); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if cache.hits != 1 {
		t.Fatalf("the second run did not read the cache: %d hits", cache.hits)
	}
	book, err := excelize.OpenReader(&xlsx)
	if err != nil {
		t.Fatal(err)
	}
	sheet := book.GetSheetName(0)
	cell, err := book.GetCellValue(sheet, "A4")
	if err != nil || cell != "P03" {
		t.Fatalf("xlsx A4 %q %v", cell, err)
	}
	capped := runner(t, analytics.RunnerOptions{StreamBatch: 4, MaxRows: 5})
	stream, err = capped.Run(w.ctx, analytics.Request{Definition: report})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		b, err := stream.Next(w.ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n += b.Len
	}
	if n != 5 || !stream.Truncated() {
		t.Fatalf("%d rows, truncated %v", n, stream.Truncated())
	}
	early, err := capped.Run(w.ctx, analytics.Request{Definition: report})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := early.Next(w.ctx); err != nil {
		t.Fatal(err)
	}
	if err := early.Close(); err != nil {
		t.Fatal(err)
	}
}
