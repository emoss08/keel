package keel

import "context"

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

func (s *scopeState) tenantPos(column string) int {
	for i, c := range s.columns {
		if c == column {
			return i
		}
	}
	return -1
}
