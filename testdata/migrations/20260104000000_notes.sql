CREATE TABLE notes (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    customer_id text NOT NULL,
    body text NOT NULL,
    search_vector tsvector GENERATED ALWAYS AS (to_tsvector('english', body)) STORED,
    CONSTRAINT pk_notes PRIMARY KEY (id, organization_id, business_unit_id)
);

CREATE INDEX idx_notes_search ON notes USING gin (search_vector);
