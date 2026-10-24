CREATE OR REPLACE VIEW tenant_item_counts AS
SELECT t.id, t.name, count(i.id) AS items
FROM tenants t
LEFT JOIN items i ON i.tenant_id = t.id
GROUP BY t.id, t.name;
