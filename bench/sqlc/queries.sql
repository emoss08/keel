-- name: GetShipment :one
SELECT id, organization_id, business_unit_id, customer_id, bill_to_id, pro_number, status, priority,
    weight, total, hazmat, labels, attributes, stage_rank, canceled_at, version, created_at, updated_at
FROM shipments
WHERE organization_id = $1 AND business_unit_id = $2 AND id = $3;

-- name: ListShipmentsByStatus :many
SELECT id, organization_id, business_unit_id, customer_id, bill_to_id, pro_number, status, priority,
    weight, total, hazmat, labels, attributes, stage_rank, canceled_at, version, created_at, updated_at
FROM shipments
WHERE organization_id = $1 AND business_unit_id = $2 AND status = $3
ORDER BY pro_number
LIMIT $4;

-- name: ListShipments :many
SELECT id, organization_id, business_unit_id, customer_id, bill_to_id, pro_number, status, priority,
    weight, total, hazmat, labels, attributes, stage_rank, canceled_at, version, created_at, updated_at
FROM shipments
WHERE organization_id = $1 AND business_unit_id = $2
ORDER BY pro_number
LIMIT $3;

-- name: CustomersByID :many
SELECT id, organization_id, business_unit_id, name, code, email, version, created_at, updated_at
FROM customers
WHERE organization_id = $1 AND business_unit_id = $2 AND id = ANY(sqlc.arg(ids)::text[]);

-- name: MovesByShipment :many
SELECT id, organization_id, business_unit_id, shipment_id, sequence, status, loaded, created_at
FROM moves
WHERE organization_id = $1 AND business_unit_id = $2 AND shipment_id = ANY(sqlc.arg(ids)::text[])
ORDER BY shipment_id, sequence;

-- name: PageShipmentsAfter :many
SELECT id, organization_id, business_unit_id, customer_id, bill_to_id, pro_number, status, priority,
    weight, total, hazmat, labels, attributes, stage_rank, canceled_at, version, created_at, updated_at
FROM shipments
WHERE organization_id = $1 AND business_unit_id = $2
    AND (created_at, id) < (sqlc.arg(after_created_at)::bigint, sqlc.arg(after_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: CountShipments :one
SELECT count(*) FROM shipments WHERE organization_id = $1 AND business_unit_id = $2;

-- name: SetOrg :exec
SELECT set_config('app.org', $1, true);

-- name: InsertCustomer :exec
INSERT INTO customers (id, organization_id, business_unit_id, name, code, email, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: CopyCustomers :copyfrom
INSERT INTO customers (id, organization_id, business_unit_id, name, code, email, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: SaveCustomer :one
UPDATE customers SET name = $4, version = version + 1, updated_at = $5
WHERE organization_id = $1 AND business_unit_id = $2 AND id = $3 AND version = $6
RETURNING version, updated_at;
