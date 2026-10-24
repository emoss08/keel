package keel_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/internal/testmodels/ids"
)

// contactNulls counts the NULL optional columns of the contact named name.
const contactNulls = `SELECT num_nulls(customer_id, phone, floor, visits, score, rating, weight, vip, status,
	priority, last_seen_at, balance, notes, prefs, avatar) FROM contacts WHERE name = $1`

// optionalColumns is the number of nullzero columns of contacts.
const optionalColumns = 15

// fullContact returns a contact with every optional field set.
func fullContact(name string, customer ids.ID) *fleet.Contact {
	return &fleet.Contact{
		Name: name, CustomerID: customer, Phone: "555-0100", Floor: 3, Visits: 7, Score: 42,
		Rating: 4.5, Weight: 1.25, VIP: true, Status: fleet.StatusAssigned, Priority: fleet.PriorityHigh,
		LastSeenAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Balance: fleet.Money{Cents: 1234},
		Notes: []string{"a", "b"}, Prefs: map[string]any{"lang": "en"}, Avatar: []byte{1, 2},
	}
}

// nulls returns how many optional columns of the contact named name are NULL.
func nulls(t testing.TB, ctx context.Context, d *keel.DB, name string) int64 {
	t.Helper()
	return must(keel.QueryValue[int64](ctx, d, contactNulls, name))(t)
}

// checkFull fails unless got carries every optional field of fullContact.
func checkFull(t testing.TB, got *fleet.Contact, customer ids.ID) {
	t.Helper()
	want := fullContact(got.Name, customer)
	eq(t, got.CustomerID, want.CustomerID)
	eq(t, got.Phone, want.Phone)
	eq(t, got.Floor, want.Floor)
	eq(t, got.Visits, want.Visits)
	eq(t, got.Score, want.Score)
	eq(t, got.Rating, want.Rating)
	eq(t, got.Weight, want.Weight)
	eq(t, got.VIP, want.VIP)
	eq(t, got.Status, want.Status)
	eq(t, got.Priority, want.Priority)
	eq(t, got.LastSeenAt.Equal(want.LastSeenAt), true)
	eq(t, got.Balance, want.Balance)
	eq(t, strings.Join(got.Notes, ","), "a,b")
	eq(t, got.Prefs["lang"], any("en"))
	eq(t, string(got.Avatar), string(want.Avatar))
}

// checkZero fails unless every optional field of got is its zero value.
func checkZero(t testing.TB, got *fleet.Contact) {
	t.Helper()
	if got.CustomerID != "" || got.Phone != "" || got.Floor != 0 || got.Visits != 0 || got.Score != 0 ||
		got.Rating != 0 || got.Weight != 0 || got.VIP || got.Status != "" || got.Priority != 0 ||
		!got.LastSeenAt.IsZero() || got.Balance != (fleet.Money{}) || got.Notes != nil || got.Prefs != nil ||
		got.Avatar != nil {
		t.Fatalf("NULL columns did not read back as zero values: %+v", got)
	}
}

func TestNullZeroCreateStoresZeroAsNull(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	empty := &fleet.Contact{Name: "empty"}
	ok(t, c.Create(ctx, d, empty))
	eq(t, nulls(t, ctx, d, "empty"), optionalColumns)
	checkZero(t, must(c.Get(ctx, d, empty.ID))(t))

	cust := newCustomer(t, ctx, "NZ")
	full := fullContact("full", cust.ID)
	ok(t, c.Create(ctx, d, full))
	eq(t, nulls(t, ctx, d, "full"), 0)
	checkFull(t, must(c.Get(ctx, d, full.ID))(t), cust.ID)
}

func TestNullZeroBulkWrites(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	cust := newCustomer(t, ctx, "NZB")
	ok(t, c.CreateMany(ctx, d, []*fleet.Contact{{Name: "many-empty"}, fullContact("many-full", cust.ID)}))
	eq(t, nulls(t, ctx, d, "many-empty"), optionalColumns)
	eq(t, nulls(t, ctx, d, "many-full"), 0)

	n := must(c.Copy(ctx, d, []*fleet.Contact{{Name: "copy-empty"}, fullContact("copy-full", cust.ID)}))(t)
	eq(t, n, 2)
	eq(t, nulls(t, ctx, d, "copy-empty"), optionalColumns)
	eq(t, nulls(t, ctx, d, "copy-full"), 0)

	rows := must(c.Where(c.Name.In("many-empty", "many-full", "copy-empty", "copy-full")).
		Order(c.Name.Asc()).All(ctx, d))(t)
	eq(t, len(rows), 4)
	checkZero(t, rows[0])
	checkFull(t, rows[1], cust.ID)
	checkZero(t, rows[2])
	checkFull(t, rows[3], cust.ID)
}

func TestNullZeroUpsertAndSave(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	cust := newCustomer(t, ctx, "NZU")
	ok(t, c.Create(ctx, d, fullContact("up", cust.ID)))

	_, err := c.Upsert(ctx, d, []*fleet.Contact{{Name: "up"}},
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Name).Update(c.Phone, c.Floor).
			Set(c.Score.To(0), c.CustomerID.To(""), c.Status.To("")))
	ok(t, err)
	got := must(c.Where(c.Name.Eq("up")).One(ctx, d))(t)
	eq(t, got.Phone, "")
	eq(t, got.Floor, int16(0))
	eq(t, got.Score, int64(0))
	eq(t, got.CustomerID, ids.ID(""))
	eq(t, got.Rating, 4.5)
	eq(t, nulls(t, ctx, d, "up"), 5)

	_, err = c.Upsert(ctx, d, []*fleet.Contact{{Name: "up"}, {Name: "up-new"}},
		keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Name))
	ok(t, err)
	eq(t, nulls(t, ctx, d, "up"), optionalColumns)
	eq(t, nulls(t, ctx, d, "up-new"), optionalColumns)

	full := fullContact("save", cust.ID)
	ok(t, c.Create(ctx, d, full))
	full.Phone, full.VIP, full.LastSeenAt = "", false, time.Time{}
	ok(t, c.Save(ctx, d, full, c.Phone, c.VIP, c.LastSeenAt))
	eq(t, nulls(t, ctx, d, "save"), 3)
	full.Notes, full.Prefs, full.Balance, full.Avatar = nil, nil, fleet.Money{}, nil
	ok(t, c.Save(ctx, d, full))
	eq(t, nulls(t, ctx, d, "save"), 7)
}

func TestNullZeroBulkUpdateAndInsertFrom(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	cust := newCustomer(t, ctx, "NZI")
	ok(t, c.Create(ctx, d, fullContact("bulk", cust.ID)))
	n := must(c.Where(c.Name.Eq("bulk")).Update(ctx, d, c.Phone.To(""), c.Rating.To(0),
		c.Weight.To(0), c.Priority.To(0), c.LastSeenAt.To(time.Time{}), c.Balance.To(fleet.Money{})))(t)
	eq(t, n, 1)
	eq(t, nulls(t, ctx, d, "bulk"), 6)

	n = must(c.InsertFrom(ctx, d,
		c.Pick(keel.Raw("'con_' || ?", c.Name), keel.Raw("? || '-copy'", c.Name), keel.Raw("''"),
			keel.Raw("0::smallint"), keel.Raw("false"), keel.Raw("'0001-01-01 00:00:00+00'::timestamptz")).
			Where(c.Name.Eq("bulk")),
		c.ID, c.Name, c.Phone, c.Floor, c.VIP, c.LastSeenAt))(t)
	eq(t, n, 1)
	eq(t, nulls(t, ctx, d, "bulk-copy"), optionalColumns)
	checkZero(t, must(c.Where(c.Name.Eq("bulk-copy")).One(ctx, d))(t))
}

func TestNullZeroPredicates(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	ok(t, c.CreateMany(ctx, d, []*fleet.Contact{
		{Name: "none"}, {Name: "a", Phone: "a", Floor: 1}, {Name: "b", Phone: "b", Floor: 2},
	}))
	count := func(p keel.Pred) int64 { return must(c.Where(p).Count(ctx, d))(t) }
	eq(t, count(c.Phone.Eq("")), 1)
	eq(t, count(c.Phone.Ne("")), 2)
	eq(t, count(c.Phone.Ne("a")), 2)
	eq(t, count(c.Phone.IsDistinctFrom("")), 2)
	eq(t, count(c.Phone.In("", "a")), 2)
	eq(t, count(c.Phone.In("a", "b")), 2)
	eq(t, count(c.Phone.NotIn("a")), 2)
	eq(t, count(c.Phone.NotIn("", "a")), 1)
	eq(t, count(c.Floor.Eq(0)), 1)
	eq(t, count(c.Floor.In(0, 2)), 2)
	eq(t, count(c.CustomerID.Eq("")), 3)
	eq(t, count(c.LastSeenAt.Eq(time.Time{})), 3)
	eq(t, count(c.VIP.Eq(false)), 3)

	sql, _ := must2(c.Where(c.Phone.Eq(""), c.Floor.Ne(0)).ToSQL(ctx, d))(t)
	if !strings.Contains(sql, `c."phone" IS NULL AND c."floor" IS NOT NULL`) {
		t.Fatalf("zero comparisons not rendered as IS NULL: %s", sql)
	}

	q := keel.DynFrom(fleetdb.ContactsSchema())
	q.Select(q.Path("name")).Where(q.Path("phone").Eq(""))
	res := must(q.Run(ctx, d))(t)
	eq(t, res.Rows, 1)
	eq(t, res.Columns[0].String[0], "none")
	q = keel.DynFrom(fleetdb.ContactsSchema())
	q.Select(q.Path("name")).Where(q.Path("floor").In([]int64{0, 1}))
	eq(t, must(q.Count(ctx, d))(t), 2)
}

func TestNullZeroRelationsAndPages(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	cust := newCustomer(t, ctx, "NZR")
	ok(t, c.CreateMany(ctx, d, []*fleet.Contact{
		{Name: "r1"}, {Name: "r2", CustomerID: cust.ID, Phone: "x"}, {Name: "r3", Phone: "y"},
	}))
	rows := must(c.Load(c.Customer()).Order(c.Name.Asc()).All(ctx, d))(t)
	eq(t, len(rows), 3)
	eq(t, rows[0].Customer == nil, true)
	eq(t, rows[1].Customer.Code, "NZR")

	var seen []string
	cursor := ""
	for {
		page := must(c.Order(c.Phone.Asc()).Page(ctx, d, keel.After(cursor, 1)))(t)
		for _, r := range page.Items {
			seen = append(seen, r.Name)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	eq(t, strings.Join(seen, ","), "r2,r3,r1")
}

// must2 is must for a function that returns two values and an error.
func must2[A, B any](a A, b B, err error) func(testing.TB) (A, B) {
	return func(t testing.TB) (A, B) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return a, b
	}
}
