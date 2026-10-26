CREATE TYPE shipment_status AS ENUM ('New', 'Assigned', 'InTransit', 'Delivered', 'Canceled');
CREATE TABLE public.customers (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    name text NOT NULL,
    code text NOT NULL,
    email text,
    version bigint DEFAULT 0 NOT NULL,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL
);
CREATE TABLE public.moves (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    shipment_id text NOT NULL,
    sequence integer NOT NULL,
    status public.shipment_status NOT NULL,
    loaded boolean DEFAULT false NOT NULL,
    created_at bigint NOT NULL
);
CREATE TABLE public.shipments (
    id text NOT NULL,
    organization_id text NOT NULL,
    business_unit_id text NOT NULL,
    customer_id text NOT NULL,
    bill_to_id text,
    pro_number text NOT NULL,
    status public.shipment_status NOT NULL,
    priority smallint DEFAULT 0 NOT NULL,
    weight bigint,
    total numeric(19,4) DEFAULT 0 NOT NULL,
    hazmat boolean DEFAULT false NOT NULL,
    labels text[] DEFAULT '{}'::text[] NOT NULL,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple'::regconfig, pro_number)) STORED,
    stage_rank smallint GENERATED ALWAYS AS (
CASE status
    WHEN 'New'::public.shipment_status THEN 1
    WHEN 'Assigned'::public.shipment_status THEN 2
    WHEN 'InTransit'::public.shipment_status THEN 3
    ELSE 4
END) STORED NOT NULL,
    canceled_at bigint,
    version bigint DEFAULT 0 NOT NULL,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL
);
ALTER TABLE ONLY public.customers
    ADD CONSTRAINT pk_customers PRIMARY KEY (id, organization_id, business_unit_id);
ALTER TABLE ONLY public.moves
    ADD CONSTRAINT pk_moves PRIMARY KEY (id, organization_id, business_unit_id);
ALTER TABLE ONLY public.shipments
    ADD CONSTRAINT pk_shipments PRIMARY KEY (id, organization_id, business_unit_id);
ALTER TABLE ONLY public.customers
    ADD CONSTRAINT uq_customers_code UNIQUE (organization_id, business_unit_id, code);
ALTER TABLE ONLY public.shipments
    ADD CONSTRAINT uq_shipments_pro UNIQUE (organization_id, business_unit_id, pro_number);
CREATE INDEX idx_moves_shipment ON public.moves USING btree (organization_id, business_unit_id, shipment_id, sequence);
CREATE INDEX idx_shipments_created ON public.shipments USING btree (organization_id, business_unit_id, created_at DESC, id DESC);
CREATE INDEX idx_shipments_status ON public.shipments USING btree (organization_id, business_unit_id, status);
ALTER TABLE ONLY public.shipments
    ADD CONSTRAINT fk_shipments_customer FOREIGN KEY (customer_id, organization_id, business_unit_id) REFERENCES public.customers(id, organization_id, business_unit_id);
