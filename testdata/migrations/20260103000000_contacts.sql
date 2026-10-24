CREATE TABLE contacts (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    customer_id text,
    name text NOT NULL,
    phone varchar(40),
    floor smallint,
    visits integer,
    score bigint,
    rating double precision,
    weight real,
    vip boolean,
    status shipment_status,
    priority smallint,
    last_seen_at timestamptz,
    balance numeric(19, 4),
    notes text[],
    prefs jsonb,
    avatar bytea,
    version bigint NOT NULL DEFAULT 0,
    CONSTRAINT pk_contacts PRIMARY KEY (id, organization_id, business_unit_id),
    CONSTRAINT uq_contacts_name UNIQUE (organization_id, business_unit_id, name),
    CONSTRAINT fk_contacts_customer FOREIGN KEY (customer_id, organization_id, business_unit_id)
        REFERENCES customers (id, organization_id, business_unit_id),
    CONSTRAINT ck_contacts_phone CHECK (phone <> ''),
    CONSTRAINT ck_contacts_score CHECK (score <> 0)
);

CREATE UNIQUE INDEX uq_contacts_name_md5 ON contacts (organization_id, business_unit_id, md5(name));
