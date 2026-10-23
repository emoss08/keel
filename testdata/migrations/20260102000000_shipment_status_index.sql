--keel:notx
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_shipments_status ON shipments (organization_id, business_unit_id, status);
--keel:split
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_workers_manager ON workers (organization_id, business_unit_id, manager_id);
