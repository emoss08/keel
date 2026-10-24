package gen

import (
	"fmt"
	"go/types"
	"slices"
	"strings"

	"github.com/emoss08/keel/schema"
)

// resolve checks every model's columns, links relations to their target models and computes
// their keys, then resolves pick relations, which walk the others.
func resolve(src *source, all map[*types.Named]*model, models []*model) error {
	for _, m := range models {
		if err := checkColumns(src, m); err != nil {
			return err
		}
	}
	for _, pick := range []bool{false, true} {
		for _, m := range models {
			for _, r := range m.rels {
				if (r.kind == schema.Pick) != pick {
					continue
				}
				if err := resolveRelation(all, m, r); err != nil {
					return src.errorf(r.pos, "%s.%s: %v", m.name, r.field, err)
				}
			}
		}
	}
	return nil
}

// resolveRelation links r to its target model and computes its keys.
func resolveRelation(all map[*types.Named]*model, m *model, r *relation) error {
	target, ok := all[r.named]
	if !ok {
		return fmt.Errorf("%s is not a model in the generated packages", r.named)
	}
	r.target = target
	switch r.kind {
	case schema.BelongsTo:
		fks, keys, err := relKeys(m, target, r.tag.RelKeys)
		if err != nil {
			return err
		}
		r.local, r.remote = fks, keys
	case schema.HasOne, schema.HasMany:
		fks, keys, err := relKeys(target, m, r.tag.RelKeys)
		if err != nil {
			return err
		}
		r.local, r.remote = keys, fks
	case schema.ManyToMany:
		pk, err := m.singleKey()
		if err != nil {
			return err
		}
		tpk, err := target.singleKey()
		if err != nil {
			return err
		}
		r.local, r.remote = []int32{pk.idx}, []int32{tpk.idx}
	case schema.Pick:
		return resolvePick(m, r)
	}
	return checkStoredKeys(m, r)
}

// checkStoredKeys refuses a relation keyed by a computed field, which is not a column of its table.
func checkStoredKeys(m *model, r *relation) error {
	for i := range r.local {
		if c := m.columns[r.local[i]]; c.tag.Flags.Has(schema.FlagComputed) {
			return fmt.Errorf("%s.%s is computed and cannot key a relation", m.name, c.field)
		}
		if c := r.target.columns[r.remote[i]]; c.tag.Flags.Has(schema.FlagComputed) {
			return fmt.Errorf("%s.%s is computed and cannot key a relation", r.target.name, c.field)
		}
	}
	return nil
}

// relKeys resolves the column pairs of a belongs, one or many relation: each foreign-key column of
// fkSide and the column of keySide it references, by default keySide's primary key for the first
// pair and the column named like the foreign key for the others. A tenant column both sides carry
// is refused, since keel adds it to every join; one that references a table without it (an
// organization_id referencing organizations.id) is an ordinary key. Pairs whose columns hold
// different kinds of value are refused.
func relKeys(fkSide, keySide *model, pairs []schema.RelKey) (fks, keys []int32, err error) {
	for i, p := range pairs {
		fk := fkSide.column(p.FK)
		if fk == nil {
			return nil, nil, fmt.Errorf("%s has no column %s", fkSide.name, p.FK)
		}
		key, err := referencedKey(keySide, p, i)
		if err != nil {
			return nil, nil, err
		}
		if sharedTenant(fk, keySide) || sharedTenant(key, fkSide) {
			return nil, nil, fmt.Errorf("%s is a tenant column; keel adds tenant columns to every join, do not list them",
				p.FK)
		}
		if keyFamily(fk.kind) != keyFamily(key.kind) {
			return nil, nil, fmt.Errorf("%s.%s and %s.%s hold different kinds of value", fkSide.name, fk.field,
				keySide.name, key.field)
		}
		fks, keys = append(fks, fk.idx), append(keys, key.idx)
	}
	return fks, keys, nil
}

// sharedTenant reports whether c is a tenant column that other also carries as a tenant column.
func sharedTenant(c *column, other *model) bool {
	if !c.tag.Flags.Has(schema.FlagTenant) {
		return false
	}
	o := other.column(c.tag.Name)
	return o != nil && o.tag.Flags.Has(schema.FlagTenant)
}

// referencedKey returns the column of keySide that relation key pair i references.
func referencedKey(keySide *model, p schema.RelKey, i int) (*column, error) {
	switch {
	case p.Key != "":
		if c := keySide.column(p.Key); c != nil {
			return c, nil
		}
		return nil, fmt.Errorf("%s has no column %s", keySide.name, p.Key)
	case i == 0:
		return keySide.singleKey()
	}
	if c := keySide.column(p.FK); c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("%s has no column %s; name the referenced column with %s=<column>", keySide.name, p.FK, p.FK)
}

// keyFamily groups the kinds whose values compare equal in a join: the integers, and every other
// kind on its own.
func keyFamily(k schema.Kind) schema.Kind {
	switch k {
	case schema.KindInt16, schema.KindInt32:
		return schema.KindInt64
	}
	return k
}

// resolvePick walks a pick relation's path, checks that it crosses a to-many relation and ends at
// the field's model, and resolves its order, appending the target's key so the order is total.
func resolvePick(m *model, r *relation) error {
	path := strings.Join(r.tag.PickPath, ".")
	hops, err := walkPick(m, r, path)
	if err != nil {
		return err
	}
	first := m.rels[r.path[0]]
	r.local = first.local
	for _, l := range r.local {
		if k := m.columns[l].kind; k != schema.KindString && k != schema.KindInt64 &&
			k != schema.KindInt32 && k != schema.KindInt16 {
			return fmt.Errorf("pick path %s starts from %s.%s, which is not a string or integer column", path,
				m.name, m.columns[l].field)
		}
	}
	key, err := r.target.singleKey()
	if err != nil {
		return err
	}
	for _, o := range r.tag.PickOrder {
		k, err := pickKey(r.tag.PickPath, hops, o)
		if err != nil {
			return err
		}
		r.order = append(r.order, k)
	}
	total := schema.PickKey{Hop: int32(len(hops) - 1), Column: key.idx}
	if i := slices.Index(r.order, total); i >= 0 && i < len(r.order)-1 {
		return fmt.Errorf("order keys after %s.%s never apply: it is %s's key", r.tag.PickPath[len(hops)-1],
			key.tag.Name, r.target.name)
	}
	if r.order[len(r.order)-1] != total {
		r.order = append(r.order, total)
	}
	for _, c := range r.tag.PickWhere {
		k, err := pickKey(r.tag.PickPath, hops, schema.PickRef{Segment: c.Segment, Column: c.Column})
		if err != nil {
			return fmt.Errorf("where: %w", err)
		}
		r.filter = append(r.filter, schema.PickFilter{Hop: k.Hop, Column: k.Column, Values: c.Values})
	}
	return nil
}

// walkPick follows a pick path from m, filling r.path, and returns the model each hop reaches.
func walkPick(m *model, r *relation, path string) ([]*model, error) {
	cur := m
	var hops []*model
	toMany := false
	for _, seg := range r.tag.PickPath {
		ri := cur.relIndex(seg)
		if ri < 0 {
			return nil, fmt.Errorf("pick path %s: %s has no relation %s", path, cur.name, seg)
		}
		hr := cur.rels[ri]
		if hr.kind == schema.Pick {
			return nil, fmt.Errorf("pick path %s: %s.%s is a pick relation", path, cur.name, hr.field)
		}
		toMany = toMany || hr.kind.ToMany()
		r.path = append(r.path, int32(ri))
		cur = hr.target
		hops = append(hops, cur)
	}
	if !toMany {
		return nil, fmt.Errorf("pick path %s crosses no to-many relation; declare belongs or one instead", path)
	}
	if cur != r.target {
		return nil, fmt.Errorf("pick path %s ends at %s, not %s", path, cur.name, r.target.name)
	}
	return hops, nil
}

// pickKey resolves one order key of a pick tag to a hop and a column of the model it reaches.
func pickKey(segs []string, hops []*model, o schema.PickRef) (schema.PickKey, error) {
	hop := slices.Index(segs, o.Segment)
	if slices.Index(segs[hop+1:], o.Segment) >= 0 {
		return schema.PickKey{}, fmt.Errorf("order key %s.%s: %s appears twice in the path", o.Segment, o.Column,
			o.Segment)
	}
	c := hops[hop].column(o.Column)
	if c == nil {
		return schema.PickKey{}, fmt.Errorf("order key %s.%s: %s has no column %s", o.Segment, o.Column,
			hops[hop].name, o.Column)
	}
	if c.tag.Flags.Has(schema.FlagComputed) {
		return schema.PickKey{}, fmt.Errorf("order key %s.%s: %s.%s is computed", o.Segment, o.Column,
			hops[hop].name, c.field)
	}
	return schema.PickKey{Hop: int32(hop), Column: c.idx}, nil
}

// relIndex returns the index of the relation whose snake_case name is seg, or -1.
func (m *model) relIndex(seg string) int {
	for i, r := range m.rels {
		if schema.Snake(r.field) == seg {
			return i
		}
	}
	return -1
}

// checkColumns refuses duplicate columns, options carried by more than one column, and options
// on columns of the wrong type.
func checkColumns(src *source, m *model) error {
	seen := map[string]bool{}
	special := map[schema.ColFlag]string{}
	for _, c := range m.columns {
		if seen[c.tag.Name] {
			return src.errorf(c.pos, "%s.%s: column %s is declared twice", m.name, c.field, c.tag.Name)
		}
		seen[c.tag.Name] = true
		for _, f := range []schema.ColFlag{schema.FlagVersion, schema.FlagAutoID, schema.FlagAutoCreate,
			schema.FlagAutoUpdate} {
			if c.tag.Flags.Has(f) {
				if prev, ok := special[f]; ok {
					return src.errorf(c.pos, "%s: %s and %s both carry the same option", m.name, prev, c.field)
				}
				special[f] = c.field
			}
		}
		if err := checkColumnType(c); err != nil {
			return src.errorf(c.pos, "%s.%s: %v", m.name, c.field, err)
		}
	}
	return nil
}

// checkColumnType refuses storage options on a column whose Go type cannot carry them.
func checkColumnType(c *column) error {
	f := c.tag.Flags
	switch {
	case f.Has(schema.FlagVersion) && (c.kind != schema.KindInt64 || c.ptr || c.wide):
		return fmt.Errorf("version columns must be int64")
	case f.Has(schema.FlagAutoID) && (c.kind != schema.KindString || c.ptr):
		return fmt.Errorf("autoid columns must be strings")
	case (f.Has(schema.FlagAutoCreate) || f.Has(schema.FlagAutoUpdate)) &&
		((c.kind != schema.KindInt64 && c.kind != schema.KindTime) || c.named != nil || c.wide):
		return fmt.Errorf("timestamp columns must be int64 (Unix seconds) or time.Time")
	}
	return nil
}
