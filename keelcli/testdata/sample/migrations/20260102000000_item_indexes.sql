--keel:notx
--keel:lock_timeout 2s
CREATE INDEX CONCURRENTLY items_tenant_idx ON items (tenant_id);
--keel:split
CREATE INDEX CONCURRENTLY items_state_idx ON items (state);
