CREATE TABLE badges (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    level integer NOT NULL DEFAULT 3,
    color text NOT NULL DEFAULT 'blue',
    note text DEFAULT 'none',
    CONSTRAINT pk_badges PRIMARY KEY (id, organization_id, business_unit_id)
);
