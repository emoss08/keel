CREATE TABLE leaves (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    worker_id text NOT NULL,
    days integer NOT NULL,
    CONSTRAINT pk_leaves PRIMARY KEY (id, organization_id, business_unit_id),
    CONSTRAINT uq_leaves_worker UNIQUE (id, worker_id, organization_id, business_unit_id)
);

CREATE TABLE leave_entries (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    worker_id text NOT NULL,
    source_leave_id text,
    hours integer NOT NULL,
    posted_at bigint NOT NULL,
    CONSTRAINT pk_leave_entries PRIMARY KEY (id, organization_id, business_unit_id)
);

CREATE INDEX idx_leave_entries_source ON leave_entries (organization_id, business_unit_id, source_leave_id, worker_id);
