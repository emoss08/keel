package keel

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
)

// render writes one statement. Clauses can be written in any order: placeholders are numbered
// by the order arguments are bound, so a clause written early keeps its numbers when the
// statement is assembled.
type render struct {
	buf     []byte
	args    []any
	err     error
	sc      *scope
	aliases []string

	scope    *scopeState
	tenantPH [8]int
	ctx      context.Context
	db       *DB
	bare     bool
	excluded bool
	free     [][]byte

	shared    []sharedArg
	sharedMap map[argKey]int
	ctes      []cteName
}

// cteName is the name a statement gives a CTE that keel names: its base name, numbered when the
// statement already holds a CTE of that base.
type cteName struct {
	ref    *Ref
	quoted string
}

// tableName returns the quoted name ref's table has in the statement: its own, or for a CTE keel
// names (a dynamic query's With, derived tables and set operations) the base name the first time
// the statement uses that base and base_2, base_3, … after, so CTEs nested in one statement never
// shadow each other. Names depend only on the statement's shape.
func (r *render) tableName(ref *Ref) string {
	if !ref.numbered {
		return ref.Table.Quoted
	}
	same := 0
	for _, n := range r.ctes {
		if n.ref == ref {
			return n.quoted
		}
		if n.ref.Table.Name == ref.Table.Name {
			same++
		}
	}
	name := ref.Table.Name
	if same > 0 {
		name += "_" + strconv.Itoa(same+1)
	}
	q := schema.Quote(name)
	r.ctes = append(r.ctes, cteName{ref: ref, quoted: q})
	return q
}

// argKey identifies a bind that may share its placeholder with an earlier one. Postgres infers a
// parameter's type from its first use, so two binds share only when both uses are known to infer
// the same type: the same bound value of the same expression (slot), an equal value compared with
// the same column through the same operator (t, col, ctx), or an equal value under the same
// explicit cast (ctx).
type argKey struct {
	v    any
	slot unsafe.Pointer
	t    *schema.Table
	col  int32
	ctx  string
}

// sharedArg is a bind that later binds may share: its key and its placeholder number.
type sharedArg struct {
	key argKey
	pos int
}

// sharedScan is the number of shareable binds looked up by a linear scan; a statement with more
// switches to a map.
const sharedScan = 16

// scope is one query level: the base table, the tables joined to it and the paths opened by
// Any/None. Column references resolve to the innermost scope that knows their Ref.
type scope struct {
	parent     *scope
	refs       []scoped
	joins      []*join
	joinMode   joinMode
	allTenants bool
	computed   []*scopedCompute
}

type joinMode uint8

const (
	joinLeft joinMode = iota
	joinFrom
	joinNone
)

type scoped struct {
	ref   *Ref
	alias string
}

type join struct {
	ref         *Ref
	alias       string
	parentAlias string
	selected    bool
	written     bool
	on          []Pred
}

// joinWritten reports whether the join with alias a has been written into the FROM clause.
func (sc *scope) joinWritten(a string) bool {
	for _, j := range sc.joins {
		if j.alias == a {
			return j.written
		}
	}
	return false
}

var renderPool = sync.Pool{New: func() any { return &render{buf: make([]byte, 0, 1024)} }}

// newRender takes a render from the pool. Callers recycle it once its statement is copied out
// with argsCopy.
func newRender(sc *scopeState, ctx context.Context, db *DB) *render {
	r := renderPool.Get().(*render)
	r.scope, r.ctx, r.db = sc, ctx, db
	return r
}

// recycle resets r and returns it to the pool.
func (r *render) recycle() {
	if cap(r.buf) > 64<<10 || cap(r.args) > 4096 {
		return
	}
	r.buf = r.buf[:0]
	clear(r.args)
	r.args = r.args[:0]
	r.err = nil
	r.sc = nil
	clear(r.aliases)
	r.aliases = r.aliases[:0]
	r.scope = nil
	r.tenantPH = [8]int{}
	r.ctx = nil
	r.db = nil
	r.bare = false
	clear(r.shared)
	r.shared = r.shared[:0]
	clear(r.ctes)
	r.ctes = r.ctes[:0]
	if len(r.sharedMap) > 4*sharedScan {
		r.sharedMap = nil
	} else if r.sharedMap != nil {
		clear(r.sharedMap)
	}
	renderPool.Put(r)
}

// argsCopy returns the bound arguments in a slice the statement owns.
func (r *render) argsCopy() []any {
	if len(r.args) == 0 {
		return nil
	}
	return append(make([]any, 0, len(r.args)), r.args...)
}

// newAlias returns base as an alias, or base followed by a number when the statement already
// uses it. Aliases depend only on the statement's shape, so equal shapes render equal text.
func (r *render) newAlias(base string) string {
	for _, u := range r.aliases {
		if u == base {
			n := 2
			for {
				cand := base + strconv.Itoa(n)
				if !r.aliasUsed(cand) {
					r.aliases = append(r.aliases, cand)
					return cand
				}
				n++
			}
		}
	}
	r.aliases = append(r.aliases, base)
	return base
}

func (r *render) aliasUsed(a string) bool {
	for _, u := range r.aliases {
		if u == a {
			return true
		}
	}
	return false
}

// aliasBase is the alias a reference asks for: its As name, its relation's alias or its table's.
func aliasBase(ref *Ref) string {
	switch {
	case ref.alias != "":
		return ref.alias
	case ref.parent != nil:
		return ref.relation().Alias
	}
	return ref.Table.Alias
}

func (r *render) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *render) s(str string) { r.buf = append(r.buf, str...) }

func (r *render) b(c byte) { r.buf = append(r.buf, c) }

func (r *render) int(n int64) { r.buf = strconv.AppendInt(r.buf, n, 10) }

func (r *render) arg(v any) {
	r.args = append(r.args, v)
	r.buf = append(r.buf, '$')
	r.buf = strconv.AppendInt(r.buf, int64(len(r.args)), 10)
}

// argCol binds v compared with column c through op, sharing the placeholder of an equal value
// bound in the same place earlier in the statement.
func (r *render) argCol(v any, c Col, op string) {
	if !shareable(v) {
		r.arg(v)
		return
	}
	r.argShared(argKey{v: v, t: c.Ref.Table, col: c.Index, ctx: op}, v)
}

// argTyped binds v under an explicit cast to typ, which the caller writes after the placeholder,
// sharing the placeholder of an equal value bound under the same cast.
func (r *render) argTyped(v any, typ string) {
	if !shareable(v) {
		r.arg(v)
		return
	}
	r.argShared(argKey{v: v, ctx: typ}, v)
}

// argSlot binds v, the value stored at slot inside an expression whose text around the value is
// ctx, sharing the placeholder of an earlier bind of the same slot in the same text: an
// expression written twice (in Pick and Group, in a CTE body and the main query) binds its
// values once. ctx keeps apart two expressions built over one shared argument slice.
func (r *render) argSlot(v any, slot unsafe.Pointer, ctx string) {
	if slot == nil {
		r.arg(v)
		return
	}
	r.argShared(argKey{slot: slot, ctx: ctx}, v)
}

// argShared writes the placeholder of the earlier bind with key k, or binds v under k.
func (r *render) argShared(k argKey, v any) {
	if r.bare {
		r.arg(v)
		return
	}
	pos := 0
	if len(r.shared) <= sharedScan {
		for i := range r.shared {
			if r.shared[i].key == k {
				pos = r.shared[i].pos
				break
			}
		}
	} else {
		pos = r.sharedMap[k]
	}
	if pos == 0 {
		r.args = append(r.args, v)
		pos = len(r.args)
		r.remember(k, pos)
	}
	r.b('$')
	r.int(int64(pos))
}

// remember records a shareable bind. Past sharedScan records the lookups move to a map.
func (r *render) remember(k argKey, pos int) {
	r.shared = append(r.shared, sharedArg{key: k, pos: pos})
	switch n := len(r.shared); {
	case n == sharedScan+1:
		if r.sharedMap == nil {
			r.sharedMap = make(map[argKey]int, 4*sharedScan)
		}
		for _, a := range r.shared {
			r.sharedMap[a.key] = a.pos
		}
	case n > sharedScan+1:
		r.sharedMap[k] = pos
	}
}

// shareable reports whether binds of v may share a placeholder by value: v is a string, integer,
// boolean, time or a named type of one of those, or a float other than zero (-0 and 0 compare
// equal) and NaN (which equals nothing). Slices, maps and other structs never share by value.
func shareable(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string, int64, int32, int16, int8, int, uint64, uint32, uint16, uint8, uint, bool, time.Time:
		return true
	case float64:
		return x != 0 && x == x
	case float32:
		return x != 0 && x == x
	}
	switch reflect.TypeOf(v).Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	case reflect.Float32, reflect.Float64:
		f := reflect.ValueOf(v).Float()
		return f != 0 && f == f
	}
	return false
}

// section runs fn with an empty buffer and returns what it wrote. The buffer comes from the
// render's free list; release returns it once the section is copied into the statement.
func (r *render) section(fn func()) []byte {
	saved := r.buf
	if n := len(r.free); n > 0 {
		r.buf = r.free[n-1][:0]
		r.free = r.free[:n-1]
	} else {
		r.buf = make([]byte, 0, 256)
	}
	fn()
	out := r.buf
	r.buf = saved
	return out
}

// release returns section buffers to the free list.
func (r *render) release(bufs ...[]byte) {
	for _, b := range bufs {
		if cap(b) > 0 && cap(b) <= 16<<10 {
			r.free = append(r.free, b)
		}
	}
}

func (r *render) push(base *Ref, mode joinMode, allTenants bool) (*scope, string) {
	alias := r.newAlias(aliasBase(base))
	sc := &scope{parent: r.sc, joinMode: mode, allTenants: allTenants}
	sc.refs = append(sc.refs, scoped{ref: base, alias: alias})
	r.sc = sc
	return sc, alias
}

func (r *render) pop() { r.sc = r.sc.parent }

// alias resolves the alias of ref, joining a belongs-to or has-one path into the scope that holds
// its parent when needed.
func (r *render) alias(ref *Ref) string {
	for sc := r.sc; sc != nil; sc = sc.parent {
		for i := range sc.refs {
			if sc.refs[i].ref == ref {
				return sc.refs[i].alias
			}
		}
	}
	if !ref.isPath() {
		if ref.alias == "" {
			if a, ok := r.sameTable(ref.Table); ok {
				return a
			}
		}
		r.fail(fmt.Errorf("keel: %s is not part of this query", ref.name()))
		return "_"
	}
	rel := ref.relation()
	if rel.Kind.ToMany() {
		r.fail(fmt.Errorf(
			"keel: %s is a to-many relation; filter it with Any or None", ref.name(),
		))
		return "_"
	}
	parentAlias := r.alias(ref.parent)
	sc := r.scopeOf(ref.parent)
	if sc == nil {
		sc = r.sc
	}
	if sc.joinMode == joinNone || (sc.joinMode == joinFrom && rel.Kind == schema.Pick) {
		r.fail(fmt.Errorf("keel: %s cannot be joined here", ref.name()))
		return "_"
	}
	return r.addJoin(sc, ref, parentAlias, false)
}

// sameTable finds the innermost reference to table t, so a column of a table's base handle
// resolves inside a statement built on a relation path to that table (a load, Any, an interceptor).
func (r *render) sameTable(t *schema.Table) (string, bool) {
	for sc := r.sc; sc != nil; sc = sc.parent {
		for i := range sc.refs {
			if sc.refs[i].ref.Table == t {
				return sc.refs[i].alias, true
			}
		}
	}
	return "", false
}

func (r *render) scopeOf(ref *Ref) *scope {
	for sc := r.sc; sc != nil; sc = sc.parent {
		for i := range sc.refs {
			if sc.refs[i].ref == ref {
				return sc
			}
		}
	}
	return nil
}

func (r *render) addJoin(sc *scope, ref *Ref, parentAlias string, selected bool) string {
	for _, j := range sc.joins {
		if j.ref == ref {
			j.selected = j.selected || selected
			return j.alias
		}
	}
	alias := r.newAlias(aliasBase(ref))
	sc.joins = append(sc.joins, &join{
		ref: ref, alias: alias, parentAlias: parentAlias, selected: selected,
	})
	sc.refs = append(sc.refs, scoped{ref: ref, alias: alias})
	return alias
}

func (r *render) col(c Col) {
	if c.Ref.Table.Columns[c.Index].Flags.Has(schema.FlagComputed) {
		r.computed(c)
		return
	}
	if r.bare {
		r.s(c.Ref.Table.Columns[c.Index].Quoted)
		return
	}
	alias := r.alias(c.Ref)
	r.s(alias)
	r.b('.')
	r.s(c.Ref.Table.Columns[c.Index].Quoted)
}

// tenantArg writes the placeholder of tenant value i, binding it on first use.
func (r *render) tenantArg(i int) {
	if r.tenantPH[i] == 0 {
		r.args = append(r.args, r.scope.tenant[i])
		r.tenantPH[i] = len(r.args)
	}
	r.b('$')
	r.int(int64(r.tenantPH[i]))
}

// tenantPred writes the tenant predicate for table t under alias, or nothing when the table has
// no tenant columns. It reports whether it wrote anything.
func (r *render) tenantPred(t *schema.Table, alias string, allTenants bool) bool {
	if !t.HasTenant() {
		return false
	}
	switch {
	case r.scope.system:
		if !allTenants {
			r.fail(fmt.Errorf("%w: %s", ErrAllTenantsRequired, t.Name))
		}
		return false
	case r.scope.tenant == nil:
		r.fail(fmt.Errorf("%w: %s", ErrNoScope, t.Name))
		return false
	case allTenants:
		r.fail(fmt.Errorf("%w: %s", ErrSystemScopeRequired, t.Name))
		return false
	}
	wrote := false
	for _, ci := range t.Tenant {
		pos := r.scope.tenantPos(t.Columns[ci].Name)
		if pos < 0 {
			r.fail(fmt.Errorf(
				"keel: tenant column %s.%s is not in Config.TenantColumns",
				t.Name, t.Columns[ci].Name,
			))
			return wrote
		}
		if wrote {
			r.s(" AND ")
		}
		r.s(alias)
		r.b('.')
		r.s(t.Columns[ci].Quoted)
		r.s(" = ")
		r.tenantArg(pos)
		wrote = true
	}
	return wrote
}

// relCond writes the join condition between the parent alias and the child alias of rel, with
// the shared tenant columns when the scope does not pin them to constants.
func (r *render) relCond(parent *schema.Table, rel *schema.Relation, parentAlias, childAlias string,
	allTenants bool,
) {
	target := rel.Target
	for i := range rel.Local {
		if i > 0 {
			r.s(" AND ")
		}
		r.s(childAlias)
		r.b('.')
		r.s(target.Columns[rel.Remote[i]].Quoted)
		r.s(" = ")
		r.s(parentAlias)
		r.b('.')
		r.s(parent.Columns[rel.Local[i]].Quoted)
	}
	r.relTenant(parent, target, parentAlias, childAlias, allTenants)
}

// relTenant writes the tenant part of a join condition after its key equality: the target's
// tenant constants under a tenant scope, its tenant columns equal to the parent's under a system
// scope.
func (r *render) relTenant(parent, target *schema.Table, parentAlias, childAlias string, allTenants bool) {
	if !target.HasTenant() {
		return
	}
	if !r.scope.system && r.scope.tenant != nil {
		r.s(" AND ")
		r.tenantPred(target, childAlias, false)
		return
	}
	if !allTenants {
		r.fail(fmt.Errorf("%w: %s", ErrAllTenantsRequired, target.Name))
		return
	}
	r.tenantEq(parent, target, parentAlias, childAlias)
}

// writeFromJoins writes the joins of sc as FROM/USING items for UPDATE and DELETE, and returns
// their join conditions for the WHERE clause.
func (r *render) writeFromJoins(sc *scope, keyword string) []byte {
	if len(sc.joins) == 0 {
		return nil
	}
	r.s(keyword)
	for i, j := range sc.joins {
		if i > 0 {
			r.s(", ")
		}
		r.s(j.ref.Table.Quoted)
		r.b(' ')
		r.s(j.alias)
	}
	return r.section(func() {
		for i, j := range sc.joins {
			if i > 0 {
				r.s(" AND ")
			}
			r.relCond(j.ref.parent.Table, j.ref.relation(), j.parentAlias, j.alias, sc.allTenants)
		}
	})
}

// preds writes ps joined by AND, skipping nil predicates. It reports whether it wrote anything.
func (r *render) preds(ps []Pred, wrote bool) bool {
	for _, p := range ps {
		if p == nil {
			continue
		}
		if wrote {
			r.s(" AND ")
		}
		p.writeSQL(r)
		wrote = true
	}
	return wrote
}

// intercepted returns the predicates interceptors add to a statement on t.
func (r *render) intercepted(t *schema.Table, op Op, read bool) []Pred {
	if r.db == nil {
		return nil
	}
	ps, err := r.db.intercept(r.ctx, t, op, read)
	if err != nil {
		r.fail(err)
	}
	return ps
}
