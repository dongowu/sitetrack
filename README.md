# SiteTrack API

Backend for SiteTrack, a construction project and materials tracker: the work
planned on each site, and the materials stored at and moved between sites.

Go 1.26 · PostgreSQL 16 · GORM · `docker compose up` and nothing else.

---

## Quick start

```bash
docker compose up -d --build          # database + migrations + API
docker compose run --rm seed --reset  # fill it with data at the brief's volume
```

The API listens on <http://localhost:8080>, everything under `/api/v1`.
`GET /api/v1/health` is public.

Override ports or the signing secret with a `.env` file (see `.env.example`).

### Accounts after seeding

All seeded accounts share the password `Password123!`.

| Email | Role |
| --- | --- |
| `superadmin@sitetrack.test` | SuperAdmin, sees every project and the company store |
| `storemanager@sitetrack.test` | Company store manager |
| `user4@sitetrack.test` … `user5000@sitetrack.test` | Ordinary users, each a member of several projects |

```bash
TOKEN=$(curl -s localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"superadmin@sitetrack.test","password":"Password123!"}' | jq -r .data.access_token)

curl -s localhost:8080/api/v1/projects -H "Authorization: Bearer $TOKEN" | jq '.meta, .data[0]'
```

The seeder is incremental: `--scale 0.01` loads a 1/100th slice in about a
second, which is what I used while developing. A full run takes roughly six
minutes on a laptop and leaves 500k tasks, 1M dependencies, 400k deliveries and
3M delivery lines in place.

---

Dependencies are vendored (`vendor/`), so the image build needs no module proxy
and cannot drift between machines.

## Repository layout

```
cmd/api            HTTP server, graceful shutdown
cmd/seed           data generator (GORM CreateInBatches)
internal/api       routing, middleware, handlers
internal/auth      password hashing, JWT access tokens, refresh token hashing
internal/config    environment configuration
internal/database  GORM models, connection, SQL migrations
docs/API.md        endpoint reference for the frontend team
scripts/smoke.sh   75 end-to-end assertions against the running API
scripts/bench.sh   reproduces the timings table under Scale
```

---

## The data model

```
users ──< sessions
  │
  ├──< project_members >── projects ──< stores (kind=project) ──< store_stock
  │                          │
  │                          ├──< invitations
  │                          ├──< tasks ──< task_deps ──< task_closure
  │                          └──< deliveries ──< delivery_lines
  │
  └───────────────────────< stores (kind=company) ──< store_stock
                                    materials ──< (referenced by stock and lines)
```

Every project owns exactly one store. The company store is a single row with
`kind = 'company'`. A delivery always joins those two kinds of store, never two
project stores.

---

## How the rules are enforced

### Identity and access

* Passwords are bcrypt (cost 12). A failed login costs the same as a successful
  one: an unknown email is compared against a dummy hash, so response time and
  message are identical.
* Access tokens are 15-minute JWTs; refresh tokens are 30-day opaque values,
  rotated on every use, only their SHA-256 stored. A person therefore never has
  to log in again while they are active.
* **Every request** re-reads the session row. `sessions.revoked_at` therefore
  kills a session on the next call, and the user's role is read fresh, so a role
  change applies immediately. The JWT deliberately carries no role or permission
  claims — a token is only proof of who you are, never of what you may do.
* A non-member gets `404`, not `403`, for every project-scoped resource. The
  same handler answers `404` for a guessed PRJ code that does not exist, so the
  API never confirms that an ID is real.
* Project admin rights, membership and the company-store gate are checked in the
  handler that owns the row, not in the router. A frontend that hides a button
  gains nothing.

### Identifiers

* `PRJ-0001` comes from a PostgreSQL sequence: `nextval` never repeats and never
  rewinds, so a deleted project's code is never handed out again.
* `PRJ-0042-T-0137` comes from a per-project counter bumped with
  `UPDATE projects SET task_seq = task_seq + 1 ... RETURNING`. The row lock is
  held to commit, so concurrent creators queue instead of colliding, and a
  rolled-back transaction also rolls the counter back.
* Internal rows use UUID primary keys. Human codes are resolved to a UUID before
  any other query runs.

### Task dependencies and status

`task_deps` is the edge table; `task_closure(ancestor, descendant, depth)` is its
transitive closure.

* Adding `B depends on A` first checks whether A already waits on B (a cycle) and
  then closes the closure in one statement: every ancestor of A reaches every
  descendant of B.
* Removing an edge or a task recomputes the closure for that one project with a
  breadth-first sweep, one depth level per statement. Deleting a node invalidates
  every path that ran through it, so the pairs cannot simply be deleted.
* Starting a task requires zero not-done ancestors. Reopening requires zero
  started descendants. Both are one indexed lookup on the closure.
* `Late` and `Blocked` are never stored. `Late` is derived from the plan dates and
  `current_date`; `Blocked` is `NOT EXISTS (not-done ancestor)`. Both are computed
  in the query, so they are always true at the moment they are read.

### Material flow and stock

* The only way material enters the system is `POST /company/store/stock`.
* A delivery takes the quantities out of the sender's stock in the same
  transaction that creates it, and setting `quantity >= $n` inside the `UPDATE`
  is what makes "a store can never go negative" hold under concurrency.
* `record` adds to the receiver, `reject` and `cancel` return to the sender. The
  delivery row is `FOR UPDATE` locked first, so a double click cannot count twice.
* Lines within one delivery are sorted by material id before touching stock, which
  is what keeps two simultaneous deliveries from deadlocking.
* `store_stock.last_delivery_at` is denormalised: computing it from 3M delivery
  lines on every store screen would not be fast.

---

## Decisions worth explaining

**GORM for the model layer, hand-written SQL for six queries.** Models, CRUD,
transactions, upserts (`ON CONFLICT`) and row locks (`clause.Locking`) are all
GORM. Six queries stay as SQL because that is the tool for the job: the project
roll-up (a CTE with `FILTER` aggregates and a window count), the task list (two
derived flags per row plus a window count), the blocked-count aggregate, the
closure insert, the closure rebuild sweep and the per-store in-transit sum.
Writing those in builder style would be slower to read and, in the closure case,
impossible to express.

**The schema lives in `migrations/*.sql`, not `AutoMigrate`.** Four of the
guarantees the brief grades cannot be expressed with struct tags: the partial
unique index that allows one open invitation per person per project, the
case-insensitive unique email, the `CHECK (quantity >= 0)`, the trigger that
keeps a dependency inside one project, and the `project_seq` sequence. AutoMigrate
would also silently drift from what those constraints enforce. The models and the
migration describe the same tables; the migration is the authority, and the API
verifies them against each other at startup by simply failing to start if a
migration cannot apply.

**Closure table instead of recursive CTEs at read time.** A project can hold
3,000 tasks and 1M dependencies exist overall. Answering "is this task blocked"
with a recursive CTE means walking the graph on every row of a list screen. The
closure turns every one of those questions into an index lookup. The cost is
write amplification on dependency changes and a bounded rebuild when a node is
deleted — a trade I would take every time, because reads outnumber graph edits by
orders of magnitude.

**SuperAdmins are treated as project admins.** The brief says a SuperAdmin reaches
every project "whether or not they are a member" but does not say whether they may
manage members. Letting them do so keeps the "at least one project admin" rule
satisfiable when a project's only admin leaves the company. Both readings are
defensible; this one is written down here so it is a decision, not an accident.

**Registration accepts `app_role`, including `superadmin`.** That is what the
brief specifies. In anything real, `superadmin` would be granted out of band.

**Notifications are invitations.** The brief's notification screen shows project
invitations, so `invitations.read_at` is the unread flag and `meta.unread_notifications`
is included in every list response — the shell can show the badge without its own
round trip. A dedicated endpoint would have been one more thing for the frontend
to call on every screen.

**Quantities are `numeric(18,3)`.** Arithmetic happens in the database, so no
float drift can accumulate; the API reads them as float8 for JSON.

---

## Scale

Seeded volume: 5,000 users · 2,000 projects · 50,000 invitations · 500,000 tasks
· 1,000,000 dependencies · 20,000 materials · 400,000 deliveries · 3,000,000
delivery lines · 2 closure rows per dependency on average.

What makes the screens hold up:

* Every project-scoped table carries `project_id` and is indexed on it, so a
  membership check and the data filter are the same predicate.
* The task list is one statement: `count(*) OVER ()` gives the total, the Late and
  Blocked flags are computed inline, and the page never fans out.
* Project blocked counts are only computed for the 20 rows on the page. The task
  counts and late counts need every visible project, so they come from one grouped
  aggregate over `tasks` restricted to the visible set.
* `deliveries` is indexed on `(project_id, created_at DESC, id DESC)` for the
  project screen, `(from_store_id, …)` and `(to_store_id, …)` for the company
  screen, and `delivery_lines` on `(material_id, delivery_id) INCLUDE (quantity)`.
  The "in transit" slice gets its own partial index,
  `deliveries (from_store_id, id) WHERE status = 'in_transit'`.
* The company store screen's "in transit" figure is a second, batched query for
  the material ids on the page — not a correlated subquery. Why is under
  *Measured timings*.

### Measured timings

`./scripts/bench.sh` reproduces this. Median of three runs, warm, single laptop,
API and Postgres in Docker on the same host:

| endpoint | median | rows / total |
| --- | --- | --- |
| `GET /projects` | 159 ms | 20 / 2002 |
| `GET /projects?sort=late_tasks&dir=desc` | 162 ms | 20 / 2002 |
| `GET /projects?offset=1000` | 105 ms | 20 / 2002 |
| `GET /projects/PRJ-0002` | 19 ms | 1 |
| `GET /projects/PRJ-0002/tasks` (3,000-task project) | 6 ms | 20 / 3000 |
| `…/tasks?blocked=true` | 9 ms | 20 / 2136 |
| `GET /projects/PRJ-0002/tasks/PRJ-0002-T-3000` | 5 ms | 1 |
| `GET /projects/PRJ-0002/store` | 6 ms | 20 / 321 |
| `GET /projects/PRJ-0002/deliveries` | 6 ms | 20 / 205 |
| `GET /company/store` | 14 ms | 20 / 20002 |
| `GET /company/store?status=low_stock` | 12 ms | 20 / 1160 |
| `GET /company/deliveries` | 60 ms | 20 / 400000 |
| `GET /materials` | 2 ms | 20 / 20002 |
| `GET /me` | 4 ms | 1 |
| `GET /notifications` | 5 ms | 13 / 13 |

The project list is the slowest read at ~160 ms, and that is honest about what it
does: to show a percentage-done and a late count for every project it has to
aggregate all 500,000 tasks, and to show blocked counts it walks 1,000,000
closure rows for the page. Splitting the work (aggregate everything, then compute
blocked for 20 rows only) is what keeps it out of the seconds.

Two things were slow enough on the first pass to be worth fixing, and both are
worth recording because the naive versions look fine:

* **Company store took 1.04 s.** `quantity_in_transit` started life as a
  correlated subquery in the page query. There is no good plan for it: driven per
  output row it re-scanned every in-transit delivery of the company store (12,673
  of them) once per row; pre-aggregated as a CTE it cost 150 ms because the whole
  store had to be summed before the page could be cut. Paginating first and then
  asking for in-transit quantities of just those 20 materials — with `OFFSET 0`
  to stop the planner from turning the semi-join back into a nested loop — plus a
  covering index on the line table and a partial index on pending deliveries
  brought it to **14 ms**. Values were checked against a hand-written aggregate.
* **`GET /materials` returned 500.** The sort expressions were written against an
  alias `m`, but `Model(&database.Material{})` names the table `materials`, so
  `ORDER BY lower(m.name)` failed with `missing FROM-clause entry for table "m"`.
  Caught by the smoke test; the expressions are now unqualified.

`001_init.sql` carries the final index shapes for a fresh database.
`002_store_indexes.sql` and `003_closure_index.sql` are idempotent upgrades for a
database created before them — they check whether an index is still the narrow
version and rebuild it only then, so no re-seed is needed either way.

## Trade-offs I accepted

* **The closure table costs writes.** A dependency edit on a 3,000-task project
  touches up to a few thousand rows, and deleting a task rebuilds that project's
  closure. Reads are what the brief measures.
* **Blocked counts on the project list are per page.** Sorting the project list by
  blocked tasks is therefore not offered; the brief only asks to sort by name,
  start date, percentage done and late tasks.
* **Delivery creation and stock deduction are one transaction**, which means a
  large delivery holds the store rows locked while it runs. At 200 lines maximum
  this is microseconds; at 20,000 it would need a different shape.
* **No event log.** The brief asks for the current state, not an audit trail, so
  deliveries do not append ledger rows.
* **The seeder does not reconcile stock with the ledger.** It writes a realistic
  current quantity per store and material, and a realistic delivery history; it
  does not replay 400k deliveries to derive the balance. Only the API moves stock,
  so anything created through the API stays consistent.

## With more time

1. **A ledger.** `stock_movement(store, material, delta, delivery_id, at)` would
   make "why is this quantity what it is" answerable and let a nightly job verify
   that `store_stock` still equals the sum of its movements.
2. **Keyset pagination** for the task and delivery lists. `OFFSET` is fine at
   400k rows and would not be at 40M.
3. **Denormalised project counters** maintained by triggers, so the project list
   stops scanning `tasks` for 2,000 projects. The numbers already agree; this is
   purely about the constant factor.
4. **A queue for the closure rebuild**, so that deleting a task in a huge project
   is not on the request path.
5. **OpenAPI generation** from the handlers, so `docs/API.md` cannot drift.
6. **Integration tests in Go** rather than a shell script. `scripts/smoke.sh` was
   the right tool to get the behaviour right quickly, but it is not what I would
   leave behind in a real repository.

## Verification

```bash
docker compose up -d
./scripts/smoke.sh     # reads API_PORT from .env
./scripts/bench.sh     # timings table under Scale
```

75 assertions covering: duplicate registration, indistinguishable login failures,
session death on logout, refresh rotation, project code allocation and
non-reuse, task code allocation, cycle rejection, self-dependency rejection, the
full status machine including the reopen guard, blocked counts before and after a
predecessor completes, stock additions, insufficient-stock rejection, delivery
record/reject/cancel and double-record, outsider 404s, invitation accept/reject,
immediate loss of access after removal, non-admin rejection, last-admin
protection, the company-store role gate.
