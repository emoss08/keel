package keel

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/emoss08/keel/schema"
)

// SetLinks makes the rows the parent row with key parent links to through the many-to-many
// relation rel exactly the rows with keys targets: one statement deletes the parent's join rows to
// every other target and inserts the missing ones (ON CONFLICT DO NOTHING, so the join table needs
// a primary key or unique index over its columns). rel is the relation's handle, as a generated
// relation method returns it (shipmentdb.Shipments.Tags()). An empty targets unlinks every row.
//
// Keys are strings or integers, or named types of them. A join table between two tenant tables is
// written under the scope's tenant: its tenant columns take the scope's values and the delete is
// limited to them, so a tenant scope is required; a system scope is refused, since the parent's
// tenant is not known. Observers see the statement with the Op "links"; the join table has no
// model, so no hook or interceptor runs, and its data version moves for the scope's tenant.
func SetLinks[K any](ctx context.Context, db *DB, rel interface{ TableRef() *Ref }, parent K,
	targets []K,
) error {
	ref := rel.TableRef()
	if ref == nil || !ref.isPath() || ref.relation().Kind != schema.ManyToMany {
		return errf("SetLinks needs the handle of a many-to-many relation")
	}
	r := ref.relation()
	owner := ref.parent.Table
	join := owner.JoinTableOf(r)
	if join == nil {
		return errf("%s.%s: SetLinks needs a single-column many-to-many relation", owner.Name, r.Name)
	}
	localKind, err := keyKindOf(owner, r.Local[0])
	if err != nil {
		return err
	}
	remoteKind, err := keyKindOf(r.Target, r.Remote[0])
	if err != nil {
		return err
	}
	parentKey, err := linkKey(parent, localKind)
	if err != nil {
		return err
	}
	keys, err := linkKeys(targets, remoteKind)
	if err != nil {
		return err
	}

	run, err := db.writeRunner(ctx, false)
	if err != nil {
		return err
	}
	defer run.abandon()
	s, err := linkStatement(run.sc, join, parentKey, keys, remoteKind)
	if err != nil {
		return run.close(ctx, err)
	}
	run.touchTable(join)
	return run.close(ctx, run.one(ctx, s, true))
}

// linkStatement renders WITH d AS (DELETE … <> ALL($2)) INSERT … SELECT … FROM unnest($2) for
// the join table under the scope's tenant.
func linkStatement(sc *scopeState, join *schema.Table, parent any, keys any, remote keyKind,
) (*stmt, error) {
	tenant := make([]string, 0, len(join.Tenant))
	args := []any{parent, keys}
	if len(join.Tenant) > 0 {
		switch {
		case sc.system:
			return nil, errf("SetLinks on %s needs a tenant scope", join.Name)
		case sc.tenant == nil:
			return nil, fmt.Errorf("%w: %s", ErrNoScope, join.Name)
		}
		for _, ci := range join.Tenant {
			pos := sc.tenantPos(join.Columns[ci].Name)
			if pos < 0 {
				return nil, errf("tenant column %s.%s is not in Config.TenantColumns", join.Name,
					join.Columns[ci].Name)
			}
			args = append(args, sc.tenant[pos])
			tenant = append(tenant, join.Columns[ci].Quoted)
		}
	}

	local, target := join.Columns[0].Quoted, join.Columns[1].Quoted
	array := "text[]"
	if remote == keyInt {
		array = "int8[]"
	}
	var b strings.Builder
	b.WriteString(`WITH "d" AS (DELETE FROM `)
	b.WriteString(join.Quoted)
	b.WriteString(" WHERE ")
	b.WriteString(local)
	b.WriteString(" = $1")
	for i, col := range tenant {
		b.WriteString(" AND ")
		b.WriteString(col)
		b.WriteString(" = $")
		b.WriteString(strconv.Itoa(i + 3))
	}
	b.WriteString(" AND ")
	b.WriteString(target)
	b.WriteString(" <> ALL($2::")
	b.WriteString(array)
	b.WriteString(")) INSERT INTO ")
	b.WriteString(join.Quoted)
	b.WriteString(" (")
	b.WriteString(local)
	b.WriteString(", ")
	b.WriteString(target)
	for _, col := range tenant {
		b.WriteString(", ")
		b.WriteString(col)
	}
	b.WriteString(") SELECT $1, \"k\"")
	for i := range tenant {
		b.WriteString(", $")
		b.WriteString(strconv.Itoa(i + 3))
	}
	b.WriteString(" FROM unnest($2::")
	b.WriteString(array)
	b.WriteString(`) AS "u"("k") ON CONFLICT DO NOTHING`)

	return &stmt{sql: b.String(), args: args, op: "links", table: join.Name}, nil
}

// linkKeys converts targets to one key array, without duplicates.
func linkKeys[K any](targets []K, kind keyKind) (any, error) {
	if kind == keyInt {
		out := make(int8Array, 0, len(targets))
		seen := make(map[int64]struct{}, len(targets))
		for _, t := range targets {
			k, err := linkKey(t, kind)
			if err != nil {
				return nil, err
			}
			n := k.(int64)
			if _, dup := seen[n]; !dup {
				seen[n] = struct{}{}
				out = append(out, n)
			}
		}
		return out, nil
	}
	out := make(textArray, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		k, err := linkKey(t, kind)
		if err != nil {
			return nil, err
		}
		s := k.(string)
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out, nil
}

// linkKey converts one key to the string or int64 of its kind.
func linkKey(key any, kind keyKind) (any, error) {
	v := reflect.ValueOf(key)
	switch {
	case kind == keyString && v.Kind() == reflect.String:
		return v.String(), nil
	case kind == keyInt && v.CanInt():
		return v.Int(), nil
	case kind == keyInt && v.CanUint():
		return int64(v.Uint()), nil //nolint:gosec // relation keys are bigint
	}
	return nil, errf("SetLinks key %v (%T) does not fit the relation's key column", key, key)
}
