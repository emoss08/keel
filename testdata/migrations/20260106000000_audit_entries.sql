CREATE TABLE audit_entries (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    tool text NOT NULL,
    args jsonb NOT NULL,
    result jsonb,
    raw json,
    meta jsonb NOT NULL,
    CONSTRAINT pk_audit_entries PRIMARY KEY (id, organization_id, business_unit_id)
);
