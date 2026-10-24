package keel

import (
	"context"
	"reflect"
)

// scopeState is the scope of one terminal call, resolved once from the context. changes collects
// the data-version pairs the call's write changed outside a transaction, until its runner closes.
type scopeState struct {
	tenant  []any
	system  bool
	reason  string
	columns []string
	changes versionSet
}

func (db *DB) resolveScope(ctx context.Context) *scopeState {
	st := &scopeState{columns: db.cfg.TenantColumns}
	if db.cfg.System != nil {
		if reason, ok := db.cfg.System(ctx); ok {
			st.system = true
			st.reason = reason
			return st
		}
	}
	if db.cfg.Tenant != nil {
		if values, ok := db.cfg.Tenant(ctx); ok && len(values) == len(db.cfg.TenantColumns) {
			st.tenant = make([]any, len(values))
			for i, v := range values {
				st.tenant[i] = normalize(v)
			}
		}
	}
	return st
}

// same reports whether s and o are the same scope: both system scopes, whatever their reasons, or
// the same tenant, or both without a scope.
func (s *scopeState) same(o *scopeState) bool {
	if s.system || o.system {
		return s.system == o.system
	}
	if len(s.tenant) != len(o.tenant) {
		return false
	}
	for i, v := range s.tenant {
		if !sameValue(v, o.tenant[i]) {
			return false
		}
	}
	return true
}

// sameValue compares two tenant values, which may be of types == cannot compare.
func sameValue(a, b any) bool {
	if as, ok := a.(string); ok {
		bs, ok := b.(string)
		return ok && as == bs
	}
	ta := reflect.TypeOf(a)
	if ta != nil && ta.Comparable() && ta == reflect.TypeOf(b) {
		return a == b
	}
	return reflect.DeepEqual(a, b)
}

func (s *scopeState) tenantPos(column string) int {
	for i, c := range s.columns {
		if c == column {
			return i
		}
	}
	return -1
}
