CREATE TABLE depots (
    id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    CONSTRAINT pk_depots PRIMARY KEY (id, business_unit_id)
);
