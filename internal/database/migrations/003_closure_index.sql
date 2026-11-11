-- task_closure_project_idx was (project_id) only. The project list reads
-- ancestor/descendant pairs for a page of projects to derive blocked counts, and
-- without the pair in the index that was ~20k scattered heap fetches per
-- request. Rebuild it as a covering index when it is still the narrow version.
--
-- Written against a database that already has the column, so it is a no-op when
-- 001_init.sql created the final shape.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_index i ON i.indexrelid = c.oid
        WHERE c.relname = 'task_closure_project_idx'
          AND i.indnatts = i.indnkeyatts
    ) THEN
        DROP INDEX task_closure_project_idx;
        CREATE INDEX task_closure_project_idx
            ON task_closure (project_id) INCLUDE (ancestor_id, descendant_id);
    END IF;
END $$;
