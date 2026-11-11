-- Upgrade path for databases created before the store projections were made
-- index-only. Everything here is idempotent: on a fresh database 001_init.sql
-- already creates the final shapes and this file does nothing.

-- Lets "pending deliveries from this store" be read index-only instead of
-- paging through the heap for every delivery.
CREATE INDEX IF NOT EXISTS deliveries_in_transit_from_idx
    ON deliveries (from_store_id, id) WHERE status = 'in_transit';

-- delivery_lines_material_idx used to be (material_id, delivery_id) only, which
-- forced a heap fetch per line whenever stock projections summed quantities.
-- Wait for it to be present at all, then rebuild it if it is still narrow.
CREATE INDEX IF NOT EXISTS delivery_lines_material_idx
    ON delivery_lines (material_id, delivery_id) INCLUDE (quantity);

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_index i ON i.indexrelid = c.oid
        WHERE c.relname = 'delivery_lines_material_idx'
          AND i.indnatts = i.indnkeyatts
    ) THEN
        DROP INDEX delivery_lines_material_idx;
        CREATE INDEX delivery_lines_material_idx
            ON delivery_lines (material_id, delivery_id) INCLUDE (quantity);
    END IF;
END $$;
