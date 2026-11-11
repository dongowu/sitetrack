-- SiteTrack initial schema.
-- Conventions:
--   * All primary keys are uuid (gen_random_uuid) so that no internal row can be
--     reached by guessing a number. Human facing identifiers (PRJ-0001) live in
--     their own columns and are resolved to a uuid before any query runs.
--   * Every table that scopes to a project carries project_id so that the
--     membership check and the data filter can be applied in one predicate.
--   * Quantities are numeric(18,3): exact decimal arithmetic in the database,
--     three decimal places is enough for construction units (bags, m3, kg, ton).

CREATE SEQUENCE project_seq START WITH 1 INCREMENT BY 1;

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL,
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    app_role      text        NOT NULL CHECK (app_role IN ('user', 'store_manager', 'superadmin')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz
);

-- Case insensitive uniqueness; a soft deleted address may be reused.
CREATE UNIQUE INDEX users_email_key ON users ((lower(email))) WHERE deleted_at IS NULL;

CREATE TABLE sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    refresh_token_hash text        NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    last_used_at       timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    revoked_at         timestamptz
);

CREATE UNIQUE INDEX sessions_refresh_key ON sessions (refresh_token_hash);
CREATE INDEX sessions_user_idx ON sessions (user_id) WHERE revoked_at IS NULL;

CREATE TABLE projects (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq           integer     NOT NULL UNIQUE,
    code          text        NOT NULL UNIQUE,
    name          text        NOT NULL,
    planned_start date        NOT NULL,
    planned_end   date        NOT NULL,
    -- Per project task counter. Bumped with a single UPDATE ... RETURNING so two
    -- concurrent task creations can never receive the same number.
    task_seq      integer     NOT NULL DEFAULT 0,
    created_by    uuid        NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT projects_dates_ok CHECK (planned_end >= planned_start)
);

CREATE INDEX projects_name_idx ON projects (lower(name));
CREATE INDEX projects_created_idx ON projects (created_at DESC);

-- One row per warehouse. kind='company' is the single central store,
-- kind='project' is the store that belongs to exactly one project.
CREATE TABLE stores (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       text        NOT NULL CHECK (kind IN ('company', 'project')),
    project_id uuid        UNIQUE REFERENCES projects (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT stores_kind_target CHECK (
        (kind = 'company' AND project_id IS NULL) OR
        (kind = 'project' AND project_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX stores_single_company ON stores (kind) WHERE kind = 'company';

CREATE TABLE project_members (
    project_id uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    is_admin   boolean     NOT NULL DEFAULT false,
    joined_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id)
);

CREATE INDEX project_members_user_idx ON project_members (user_id);

CREATE TABLE invitations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    inviter_id   uuid        NOT NULL REFERENCES users (id),
    invitee_id   uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status       text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'accepted', 'rejected', 'withdrawn')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    responded_at timestamptz,
    read_at      timestamptz
);

-- A person has at most one open invitation per project.
CREATE UNIQUE INDEX invitations_open_key ON invitations (project_id, invitee_id) WHERE status = 'pending';
CREATE INDEX invitations_invitee_idx ON invitations (invitee_id, created_at DESC);
CREATE INDEX invitations_unread_idx ON invitations (invitee_id) WHERE read_at IS NULL AND status = 'pending';
CREATE INDEX invitations_project_idx ON invitations (project_id, status, created_at DESC);

CREATE TABLE tasks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    seq           integer     NOT NULL,
    code          text        NOT NULL,
    name          text        NOT NULL,
    planned_start date        NOT NULL,
    planned_end   date        NOT NULL,
    status        text        NOT NULL DEFAULT 'not_started'
        CHECK (status IN ('not_started', 'in_progress', 'done')),
    started_at    timestamptz,
    done_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tasks_seq_unique UNIQUE (project_id, seq),
    CONSTRAINT tasks_code_unique UNIQUE (project_id, code),
    CONSTRAINT tasks_dates_ok CHECK (planned_end >= planned_start)
);

CREATE INDEX tasks_project_status_idx ON tasks (project_id, status);
CREATE INDEX tasks_project_start_idx ON tasks (project_id, planned_start);
CREATE INDEX tasks_project_end_idx ON tasks (project_id, planned_end);
CREATE INDEX tasks_project_name_idx ON tasks (project_id, lower(name));

-- task_id waits for depends_on_id. Project stores never talk to each other, so
-- both ends are forced into the same project by the application and re-checked
-- by the trigger below.
CREATE TABLE task_deps (
    project_id    uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    task_id       uuid        NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    depends_on_id uuid        NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, depends_on_id),
    CONSTRAINT task_deps_no_self CHECK (task_id <> depends_on_id)
);

CREATE INDEX task_deps_depends_on_idx ON task_deps (depends_on_id, task_id);

-- Transitive closure of task_deps, one row per reachable pair.
-- depth 1 means "directly depends on". Maintained incrementally on insert and
-- recomputed per project on delete (deletes are rare, and removing a node
-- invalidates every path that ran through it).
CREATE TABLE task_closure (
    project_id    uuid NOT NULL,
    ancestor_id   uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    descendant_id uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    depth         integer NOT NULL,
    PRIMARY KEY (ancestor_id, descendant_id),
    CONSTRAINT task_closure_no_self CHECK (ancestor_id <> descendant_id)
);

CREATE INDEX task_closure_descendant_idx ON task_closure (descendant_id, depth, ancestor_id);
-- INCLUDE keeps per-project blocked counts index-only: the project list asks for
-- ancestor/descendant pairs of 20 projects at a time, and reading them from the
-- heap cost ~20k scattered page fetches per request.
CREATE INDEX task_closure_project_idx
    ON task_closure (project_id) INCLUDE (ancestor_id, descendant_id);

CREATE TABLE materials (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    category   text        NOT NULL,
    unit       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX materials_name_key ON materials ((lower(name)));
CREATE INDEX materials_category_idx ON materials (category);

CREATE TABLE store_stock (
    store_id         uuid           NOT NULL REFERENCES stores (id) ON DELETE CASCADE,
    material_id      uuid           NOT NULL REFERENCES materials (id) ON DELETE CASCADE,
    quantity         numeric(18, 3) NOT NULL DEFAULT 0,
    -- Only meaningful for the company store; null elsewhere.
    reorder_point    numeric(18, 3),
    -- Denormalised "date of last delivery in or out" for the store screen. Kept
    -- in sync by the delivery transitions instead of scanning 3M delivery lines.
    last_delivery_at timestamptz,
    updated_at       timestamptz    NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, material_id),
    CONSTRAINT store_stock_non_negative CHECK (quantity >= 0)
);

CREATE INDEX store_stock_material_idx ON store_stock (material_id);

CREATE TABLE deliveries (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    from_store_id uuid        NOT NULL REFERENCES stores (id),
    to_store_id   uuid        NOT NULL REFERENCES stores (id),
    status        text        NOT NULL DEFAULT 'in_transit'
        CHECK (status IN ('in_transit', 'delivered', 'rejected', 'cancelled')),
    created_by    uuid        NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at   timestamptz,
    resolved_by   uuid REFERENCES users (id),
    CONSTRAINT deliveries_distinct_stores CHECK (from_store_id <> to_store_id)
);

CREATE INDEX deliveries_project_idx ON deliveries (project_id, created_at DESC, id DESC);
CREATE INDEX deliveries_from_idx ON deliveries (from_store_id, created_at DESC);
CREATE INDEX deliveries_to_idx ON deliveries (to_store_id, created_at DESC);
CREATE INDEX deliveries_status_idx ON deliveries (status, created_at DESC);

-- The company store screens show "how much of this material is on a truck that
-- left us", which is answered from the ledger. In transit is a small, hot slice
-- of deliveries, so it gets its own partial index: it lets the store's pending
-- deliveries be read index-only, without touching the heap of 400k rows.
CREATE INDEX deliveries_in_transit_from_idx ON deliveries (from_store_id, id)
    WHERE status = 'in_transit';

CREATE TABLE delivery_lines (
    delivery_id uuid           NOT NULL REFERENCES deliveries (id) ON DELETE CASCADE,
    material_id uuid           NOT NULL REFERENCES materials (id),
    quantity    numeric(18, 3) NOT NULL,
    PRIMARY KEY (delivery_id, material_id),
    CONSTRAINT delivery_lines_positive CHECK (quantity > 0)
);

-- INCLUDE (quantity) makes the stock projections index-only. Without it the
-- ~150 delivery lines per material each cost a scattered heap fetch.
CREATE INDEX delivery_lines_material_idx
    ON delivery_lines (material_id, delivery_id) INCLUDE (quantity);

-- Keep deps inside one project even if a caller tries to bypass the API.
CREATE OR REPLACE FUNCTION task_deps_same_project() RETURNS trigger AS $$
BEGIN
    IF (SELECT project_id FROM tasks WHERE id = NEW.task_id) <> NEW.project_id
       OR (SELECT project_id FROM tasks WHERE id = NEW.depends_on_id) <> NEW.project_id THEN
        RAISE EXCEPTION 'dependency crosses project boundary';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER task_deps_same_project_trg
    BEFORE INSERT OR UPDATE ON task_deps
    FOR EACH ROW EXECUTE FUNCTION task_deps_same_project();
