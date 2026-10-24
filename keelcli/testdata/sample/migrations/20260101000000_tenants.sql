CREATE TABLE tenants (
    id text PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE states (
    code text PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE tenant_settings (
    tenant_id text NOT NULL REFERENCES tenants (id),
    key text NOT NULL,
    value text NOT NULL,
    PRIMARY KEY (tenant_id, key)
);

CREATE TABLE items (
    id serial PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants (id),
    name text NOT NULL,
    state text REFERENCES states (code)
);

INSERT INTO tenants (id, name) VALUES ('t1', 'Acme'), ('t2', 'Globex'), ('t3', 'Initech');
