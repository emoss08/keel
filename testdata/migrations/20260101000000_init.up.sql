CREATE TYPE shipment_status AS ENUM ('New', 'Assigned', 'InTransit', 'Delivered', 'Canceled');

CREATE TABLE customers (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    code text NOT NULL,
    email text,
    version bigint NOT NULL DEFAULT 0,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT pk_customers PRIMARY KEY (id, organization_id, business_unit_id),
    CONSTRAINT uq_customers_code UNIQUE (organization_id, business_unit_id, code)
);

CREATE TABLE shipments (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    customer_id text NOT NULL,
    bill_to_id text,
    pro_number text NOT NULL,
    status shipment_status NOT NULL,
    priority smallint NOT NULL DEFAULT 0,
    weight bigint,
    total numeric(19, 4) NOT NULL DEFAULT 0,
    hazmat boolean NOT NULL DEFAULT false,
    labels text[] NOT NULL DEFAULT '{}',
    attributes jsonb NOT NULL DEFAULT '{}',
    search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple', pro_number)) STORED,
    stage_rank smallint GENERATED ALWAYS AS (
        CASE status WHEN 'New' THEN 1 WHEN 'Assigned' THEN 2 WHEN 'InTransit' THEN 3 ELSE 4 END
    ) STORED,
    canceled_at bigint,
    version bigint NOT NULL DEFAULT 0,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT pk_shipments PRIMARY KEY (id, organization_id, business_unit_id),
    CONSTRAINT uq_shipments_pro UNIQUE (organization_id, business_unit_id, pro_number),
    CONSTRAINT fk_shipments_customer FOREIGN KEY (customer_id, organization_id, business_unit_id)
        REFERENCES customers (id, organization_id, business_unit_id)
);

CREATE INDEX idx_shipments_created ON shipments (organization_id, business_unit_id, created_at DESC, id DESC);

CREATE TABLE moves (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    shipment_id text NOT NULL,
    sequence integer NOT NULL,
    status shipment_status NOT NULL,
    loaded boolean NOT NULL DEFAULT false,
    created_at bigint NOT NULL,
    CONSTRAINT pk_moves PRIMARY KEY (id, organization_id, business_unit_id)
);

CREATE INDEX idx_moves_shipment ON moves (organization_id, business_unit_id, shipment_id, sequence);

CREATE TABLE locations (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    city text NOT NULL,
    CONSTRAINT pk_locations PRIMARY KEY (id, organization_id, business_unit_id)
);

CREATE TABLE stops (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    move_id text NOT NULL,
    location_id text NOT NULL,
    sequence integer NOT NULL,
    kind text NOT NULL,
    arrived_at timestamptz,
    CONSTRAINT pk_stops PRIMARY KEY (id, organization_id, business_unit_id)
);

CREATE TABLE tags (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    CONSTRAINT pk_tags PRIMARY KEY (id, organization_id, business_unit_id)
);

CREATE TABLE shipment_tags (
    shipment_id text NOT NULL,
    tag_id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    CONSTRAINT pk_shipment_tags PRIMARY KEY (shipment_id, tag_id, organization_id, business_unit_id)
);

CREATE TABLE workers (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    manager_id text,
    first_name text NOT NULL,
    last_name text NOT NULL,
    whole_name text GENERATED ALWAYS AS (first_name || ' ' || last_name) STORED,
    hired_at timestamptz NOT NULL,
    CONSTRAINT pk_workers PRIMARY KEY (id, organization_id, business_unit_id)
);

CREATE TABLE plants (
    code text PRIMARY KEY,
    name text NOT NULL
);
