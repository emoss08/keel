package keel_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/internal/testmodels/fleet"
	"github.com/emoss08/keel/internal/testmodels/fleet/fleetdb"
	"github.com/emoss08/keel/schema"
)

func constraintErr(t *testing.T, err error) *keel.ConstraintError {
	t.Helper()
	var ce *keel.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want a *keel.ConstraintError", err)
	}
	return ce
}

func TestViolationsResolveToConstraintErrors(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	ok(t, c.Create(ctx, d, &fleet.Contact{Name: "taken"}))

	dup := constraintErr(t, c.Create(ctx, d, &fleet.Contact{Name: "taken"}))
	eq(t, dup.Kind, keel.ConstraintUnique)
	eq(t, dup.Table, "contacts")
	eq(t, dup.Constraint, "uq_contacts_name")
	eq(t, strings.Join(dup.Columns, ","), "name")
	eq(t, dup.Message, "A contact with this name already exists")
	eq(t, dup.Model, fleetdb.ContactsSchema())
	eq(t, keel.IsUnique(dup, "uq_contacts_name"), true)
	eq(t, dup.Kind.String(), "unique")

	fk := constraintErr(t, c.Create(ctx, d, &fleet.Contact{Name: "orphan", CustomerID: "cus_missing"}))
	eq(t, fk.Kind, keel.ConstraintForeignKey)
	eq(t, strings.Join(fk.Columns, ","), "customer_id")
	eq(t, fk.Message, "The customer does not exist")

	_, err := c.Where(c.Name.Eq("taken")).Update(ctx, d, c.Score.ToExpr(keel.DynConst(0)))
	chk := constraintErr(t, err)
	eq(t, chk.Kind, keel.ConstraintCheck)
	eq(t, chk.Constraint, "ck_contacts_score")
	eq(t, strings.Join(chk.Columns, ","), "score")
	eq(t, chk.Message, "")

	_, err = c.Where(c.Name.Eq("taken")).Update(ctx, d, c.Name.ToNull())
	nn := constraintErr(t, err)
	eq(t, nn.Kind, keel.ConstraintNotNull)
	eq(t, strings.Join(nn.Columns, ","), "name")
	if len(nn.Labels) != 1 || nn.Labels[0] == "" {
		t.Fatalf("labels %v", nn.Labels)
	}

	err = keel.Write(ctx, d, func(ctx context.Context) error {
		return c.Create(ctx, d, &fleet.Contact{Name: "taken"})
	})
	eq(t, constraintErr(t, err).Constraint, "uq_contacts_name")
	_, err = keel.Exec(ctx, d, "INSERT INTO contacts (id, organization_id, business_unit_id, name) "+
		"SELECT 'con_x', organization_id, business_unit_id, name FROM contacts WHERE name = 'taken'")
	eq(t, constraintErr(t, err).Kind, keel.ConstraintUnique)
}

func TestVerifyFindsAConstraintTheModelNamesButTheTableLacks(t *testing.T) {
	d := db(t)
	tb := *fleetdb.ContactsSchema()
	tb.Constraints = append([]schema.Constraint{{Name: "uq_contacts_nickname", Message: "x"}}, tb.Constraints...)
	drifts := must(keel.Verify(context.Background(), d, []*schema.Table{&tb}))(t)
	eq(t, len(drifts), 1)
	eq(t, drifts[0].Kind, keel.DriftConstraint)
	if !strings.Contains(drifts[0].Detail, "uq_contacts_nickname") {
		t.Fatalf("drift %s", drifts[0])
	}
}

func TestExpressionAndPartialIndexesResolveTheColumnsTheyRead(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	b := fleetdb.Badges
	ok(t, b.Create(ctx, d, &fleet.Badge{Name: "Gold", Color: "gold"}))
	byName := constraintErr(t, b.Create(ctx, d, &fleet.Badge{Name: "GOLD"}))
	eq(t, byName.Constraint, "uq_badges_name")
	eq(t, strings.Join(byName.Columns, ","), "name")
	partial := constraintErr(t, b.Create(ctx, d, &fleet.Badge{Name: "Other", Color: "gold"}))
	eq(t, partial.Constraint, "uq_badges_one_gold")
	eq(t, strings.Join(partial.Columns, ","), "color")
}

func TestADeclaredFieldReplacesTheResolvedOne(t *testing.T) {
	d := db(t)
	ctx := newTenant()
	c := fleetdb.Contacts
	ok(t, c.Create(ctx, d, &fleet.Contact{Name: "x"}))
	tb := fleetdb.ContactsSchema()
	saved := tb.Constraints
	t.Cleanup(func() { tb.Constraints = saved })
	tb.Constraints = []schema.Constraint{{Name: "uq_contacts_name", Message: "taken", Field: "phone"}}
	ce := constraintErr(t, c.Create(ctx, d, &fleet.Contact{Name: "x"}))
	eq(t, strings.Join(ce.Fields, ","), "phone")
	eq(t, ce.Message, "taken")
}
