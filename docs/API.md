# SiteTrack API reference

Base URL: `http://localhost:8080/api/v1`

Everything is JSON. Every request except register, login, refresh and health must
carry `Authorization: Bearer <access_token>`.

---

## Conventions

### Response envelope

Single resource:

```json
{ "data": { "id": "PRJ-0042", "name": "Riverside Apartments 3" } }
```

List:

```json
{
  "data": [ ... ],
  "meta": { "total": 42, "limit": 20, "offset": 0, "unread_notifications": 3 }
}
```

`meta.total` is the number of rows matching the filter, not the page size.
`meta.unread_notifications` is present on the list screens so the shell can render
the notification badge without a second call.

### Errors

```json
{ "error": { "code": "not_found", "message": "project not found" } }
```

| Status | `code` | When |
| --- | --- | --- |
| 400 | `invalid_request` | Malformed body, bad date, missing field |
| 401 | `unauthorized` | Missing, expired or revoked token; wrong password |
| 403 | `forbidden` | Authenticated, but this role may not do it |
| 404 | `not_found` | Missing — **or not visible to you** |
| 409 | `conflict` | Duplicate, insufficient stock, already-answered invitation |
| 422 | `rule_violation` | A domain rule said no (cycle, blocked task, bad transition) |
| 500 | `internal_error` | Bug; the detail is only in the server log |

A project you are not a member of answers `404`, exactly like a project code that
does not exist. Do not build UI that distinguishes them; there is nothing to
distinguish.

### Paging, filtering, sorting

* `limit` (default 20, max 100) and `offset`.
* `q` performs a case-insensitive substring search on the fields named per endpoint.
* `sort` and `dir` (`asc`/`desc`) accept only the values listed per endpoint;
  anything else falls back to the default.
* Date filters take `YYYY-MM-DD`. Date ranges are inclusive at both ends.

### Identifiers

* Projects are addressed by their code: `PRJ-0042`.
* Tasks are addressed by their code: `PRJ-0042-T-0137`. `T-0137` and `137` are
  accepted as shorthand.
* Materials, invitations and deliveries are addressed by UUID.

### Roles

| `app_role` | Reaches |
| --- | --- |
| `user` | the projects they are a member of |
| `store_manager` | the same, plus the company store |
| `superadmin` | every project and the company store, member or not |

Within a project, `is_project_admin` additionally allows managing members and
invitations. Creating a project makes you its admin. A project always keeps at
least one admin.

---

## Auth

### `POST /auth/register`

```json
{ "name": "Aisha Abbas", "email": "aisha@example.com", "password": "Password123!", "app_role": "user" }
```

`app_role` is optional and defaults to `user`; one of `user`, `store_manager`,
`superadmin`. Password must be at least 8 characters.

`201` — logs the new account in immediately:

```json
{
  "data": {
    "user": { "id": "…", "name": "Aisha Abbas", "email": "aisha@example.com", "app_role": "user", "created_at": "2026-11-11T14:00:00Z" },
    "access_token": "eyJ…",
    "refresh_token": "0Yb…",
    "expires_in": 900
  }
}
```

`409` if the email is taken.

### `POST /auth/login`

```json
{ "email": "aisha@example.com", "password": "Password123!" }
```

Same body as register minus the name and role. `401` with
`"email or password is incorrect"` for both an unknown email and a wrong
password — the response never reveals which.

### `POST /auth/refresh`

```json
{ "refresh_token": "0Yb…" }
```

`200 { "data": { "access_token": "…", "refresh_token": "…", "expires_in": 900 } }`

The refresh token is rotated: the old one stops working the moment this succeeds.

### `POST /auth/logout`

Requires auth. `204`. Revokes the session behind the access token you used. The
same access token is rejected on the very next request, and its refresh token with
it.

### `GET /me`

```json
{
  "data": {
    "user": { "id": "…", "name": "…", "email": "…", "app_role": "user", "created_at": "…" },
    "project_count": 4,
    "pending_invitations": 1,
    "unread_notifications": 1
  }
}
```

### `PATCH /me`

```json
{ "name": "Aisha A.", "email": "new@example.com" }
```

Both optional. `409` if the new email is taken.

### `POST /me/password`

```json
{ "current_password": "Password123!", "new_password": "EvenBetter123!" }
```

`204`. Every other session of this user is revoked.

---

## Notifications and invitations

### `GET /notifications`

Query: `status` (`pending`, `accepted`, `rejected`, `withdrawn`, `all`), `limit`, `offset`.

```json
{
  "data": [
    {
      "id": "9f1c…",
      "type": "project_invitation",
      "project_id": "PRJ-0042",
      "project_name": "Riverside Apartments 3",
      "inviter_name": "Chen Costa",
      "status": "pending",
      "unread": true,
      "created_at": "2026-11-09T08:12:00Z",
      "can_respond": true
    }
  ],
  "meta": { "total": 5, "limit": 20, "offset": 0, "unread_notifications": 2 }
}
```

Newest first. `can_respond` is `true` only while the invitation is pending.

### `POST /notifications/read`

`200 { "data": { "unread_notifications": 0 } }`. Marks every pending invitation
of this user as read.

### `POST /invitations/{id}/accept` · `POST /invitations/{id}/reject`

Only the invitee can answer, and only while pending.

`200 { "data": { "project_id": "PRJ-0042", "status": "accepted" } }`

Accepting creates the membership. `404` if the invitation is not yours, `409` if it
was already answered or withdrawn.

---

## Materials

The catalogue is shared by every logged-in user.

### `GET /materials`

Query: `q`, `category`, `sort` (`name` · `category` · `unit`), `dir`, `limit`, `offset`.

```json
{ "data": [ { "id": "…", "name": "Cement, OPC 42.5 50kg bag", "category": "Cement", "unit": "bag" } ],
  "meta": { "total": 20000, "limit": 20, "offset": 0 } }
```

### `POST /materials`

```json
{ "name": "Cement, OPC 42.5 50kg bag", "category": "Cement", "unit": "bag" }
```

`201` with the material. `409` if the name (case-insensitive) exists.

### `PATCH /materials/{id}` · `DELETE /materials/{id}`

Partial update; `204` on delete. Deleting a material that a store or a delivery
still references answers `409` rather than erasing history.

---

## Users

### `GET /users?q=`

Admin helper for inviting people. `q` must be at least 2 characters; returns up
to 20 matches on name or email with `id`, `name`, `email`, `app_role`.

---

## Projects

### `POST /projects`

```json
{ "name": "Riverside Apartments 3", "planned_start": "2026-01-05", "planned_end": "2026-12-20" }
```

`201`:

```json
{
  "data": {
    "id": "PRJ-0042",
    "name": "Riverside Apartments 3",
    "planned_start": "2026-01-05",
    "planned_end": "2026-12-20",
    "is_project_admin": true,
    "member_count": 1,
    "task_counts": { "total": 0, "not_started": 0, "in_progress": 0, "done": 0 },
    "percent_done": 0,
    "late_task_count": 0,
    "blocked_task_count": 0,
    "created_at": "2026-11-11T14:00:00Z"
  }
}
```

### `GET /projects`

Query: `q` (code or name), `has_late_tasks` (bool), `sort`
(`name` · `start_date` · `percent_done` · `late_tasks` · `id` · `created_at`,
default `created_at`), `dir`, `limit`, `offset`.

Each row is the shape above. `percent_done` is `done / total × 100`, `0` when the
project has no tasks.

### `GET /projects/{code}`

The same object for one project.

### `PATCH /projects/{code}` · `DELETE /projects/{code}`

Project admin only. Partial body: `name`, `planned_start`, `planned_end`.
`204` on delete; the `PRJ` code is never reissued.

---

## Members and invitations

### `GET /projects/{code}/members`

Any member.

```json
{
  "data": {
    "project_id": "PRJ-0042",
    "is_project_admin": true,
    "members": [
      { "user_id": "…", "name": "Chen Costa", "email": "…", "app_role": "user", "is_project_admin": true, "joined_at": "…" }
    ],
    "pending_invitations": [
      { "id": "…", "status": "pending", "invitee_id": "…", "invitee_name": "…", "invitee_email": "…", "created_at": "…" }
    ]
  }
}
```

Admins first, then by name.

### `POST /projects/{code}/invitations`

Project admin only.

```json
{ "email": "someone@example.com" }
```

`user_id` may be sent instead of `email`. `201` with the invitation. `404` if no
such registered user, `409` if they are already a member or already have an open
invitation.

The person joins only when they accept.

### `DELETE /projects/{code}/invitations/{id}`

Project admin withdraws an unanswered invitation. `204`.

### `PATCH /projects/{code}/members/{userID}`

Project admin only.

```json
{ "is_project_admin": true }
```

`409` if demoting would leave the project with no admin.

### `DELETE /projects/{code}/members/{userID}`

Project admin only. `204`. Takes effect on the removed person's next request.
`409` if it would leave the project with no admin.

---

## Tasks

### `GET /projects/{code}/tasks`

Query: `status` (`not_started` · `in_progress` · `done`), `late` (bool),
`blocked` (bool), `from` / `to` (planned-date window, matched against the task's
plan span), `q` (code or name), `sort` (`id` · `name` · `start_date` ·
`end_date` · `dependents`, default `id`), `dir`, `limit`, `offset`.

```json
{
  "data": [
    {
      "id": "PRJ-0042-T-0137",
      "project_id": "PRJ-0042",
      "name": "Pour level 2 slab",
      "planned_start": "2026-02-01",
      "planned_end": "2026-02-09",
      "status": "in_progress",
      "is_late": false,
      "is_blocked": false,
      "depends_on": ["PRJ-0042-T-0136"],
      "dependent_count": 3,
      "started_at": "2026-02-02T06:00:00Z",
      "done_at": null
    }
  ],
  "meta": { "total": 250, "limit": 20, "offset": 0, "unread_notifications": 1 }
}
```

`is_late` and `is_blocked` are derived, never stored:

* **Late** — not done, and either its planned end has passed, or it has not
  started and its planned start has passed.
* **Blocked** — not done, and at least one task it waits on, directly or
  indirectly, is not done.

A task can be both.

### `POST /projects/{code}/tasks`

```json
{ "name": "Pour level 2 slab", "planned_start": "2026-02-01", "planned_end": "2026-02-09" }
```

`201`. The code is allocated inside the project: `PRJ-0042-T-0137`. New tasks
start as `not_started`.

### `GET /projects/{code}/tasks/{taskCode}`

```json
{
  "data": {
    "task": { "…the list row, plus allowed_actions…" },
    "waits_on": [
      { "id": "PRJ-0042-T-0136", "name": "…", "status": "done", "planned_start": "…", "planned_end": "…", "steps": 1 },
      { "id": "PRJ-0042-T-0100", "name": "…", "status": "done", "planned_start": "…", "planned_end": "…", "steps": 2 }
    ],
    "blocks": [ { "…": "…", "steps": 1 } ]
  }
}
```

`waits_on` is every task this one depends on, directly or indirectly, with the
shortest number of steps; `blocks` is every task that waits on it, same shape.
`allowed_actions` is the subset of `start`, `done`, `reopen` that is legal right
now — render exactly those buttons.

### `PATCH /projects/{code}/tasks/{taskCode}` · `DELETE ...`

Partial update of `name`, `planned_start`, `planned_end`; `204` on delete.
Deleting a task removes its dependencies and rebuilds the project's graph.

### Status actions

| Endpoint | From | Rule |
| --- | --- | --- |
| `POST .../start` | `not_started` | every task it waits on must be done |
| `POST .../done` | `in_progress` | — |
| `POST .../reopen` | `done` | no task that waits on it may have started |

Each returns the task-detail object. A rejected transition is `422` with a message
naming the count, e.g. `"this task still waits on 2 task(s) that are not done"`.

### Dependencies

`POST /projects/{code}/tasks/{taskCode}/dependencies`

```json
{ "depends_on": "PRJ-0042-T-0001" }
```

Means: this task cannot start until `PRJ-0042-T-0001` is done. `200` with the
updated task detail. `422` for a self-dependency or for anything that would close
a loop (the message names both ends).

`DELETE /projects/{code}/tasks/{taskCode}/dependencies/{dependsOnCode}` — `200`
with the updated task detail.

Dependencies never cross a project boundary; the API checks it and a trigger
enforces it.

---

## Project store

### `GET /projects/{code}/store`

Query: `q` (name), `category`, `sort` (`name` · `quantity` · `last_delivery` ·
`category`), `dir`, `limit`, `offset`.

```json
{
  "data": [
    { "material_id": "…", "name": "Cement, OPC 42.5 50kg bag", "category": "Cement",
      "unit": "bag", "quantity": 120, "last_delivery_at": "2026-11-02T09:31:00Z" }
  ],
  "meta": { "total": 214, "limit": 20, "offset": 0, "unread_notifications": 0 }
}
```

`last_delivery_at` is the date of the most recent delivery in or out for that
material at this store.

---

## Project deliveries

A delivery moves material between the company store and one project store, in
either direction. Status: `in_transit` → `delivered` | `rejected` | `cancelled`.

### `GET /projects/{code}/deliveries`

Query: `direction` (`in` · `out`), `status`, `from`, `to`, `limit`, `offset`.
Newest first.

```json
{
  "data": [
    {
      "id": "…",
      "project_id": "PRJ-0042",
      "project_name": "Riverside Apartments 3",
      "direction": "incoming",
      "from": "company_store",
      "to": "project_store",
      "status": "in_transit",
      "lines": [ { "material_id": "…", "name": "…", "unit": "bag", "quantity": 120 } ],
      "created_by": "Chen Costa",
      "created_at": "2026-11-09T08:12:00Z",
      "resolved_at": null,
      "resolved_by": null,
      "allowed_actions": ["record", "reject"]
    }
  ],
  "meta": { "total": 137, "limit": 20, "offset": 0, "unread_notifications": 2 }
}
```

`direction`, `from` and `to` are relative to the store you are looking at.

### `POST /projects/{code}/deliveries`

Sends material **from the project store back to the company store**.

```json
{ "lines": [ { "material_id": "…", "quantity": 50 } ] }
```

`201`. The quantity leaves the project store immediately and the delivery becomes
`in_transit`. `409` if the store does not hold enough. `400` for a duplicate
material in one delivery, a non-positive quantity, or more than 200 lines.

### `POST /projects/{code}/deliveries/{id}/record` · `.../reject` · `.../cancel`

* `record` — receiver only. Adds the quantities to the project store, `delivered`.
* `reject` — receiver only. Returns them to the sender, `rejected`.
* `cancel` — sender only, while in transit. Returns them, `cancelled`.

Each returns the delivery. `409` if it is no longer in transit (a second
`record` cannot double count), `403` if you are the wrong end.

### `GET /projects/{code}/deliveries/{id}`

The same object for one delivery.

---

## Company store

Company store managers and SuperAdmins only; everyone else gets `403`.

### `GET /company/store`

Query: `status` (`out_of_stock` · `low_stock` · `available`), `category`, `q`,
`sort` (`name` · `quantity` · `status` · `category`), `dir`, `limit`, `offset`.

```json
{
  "data": [
    { "material_id": "…", "name": "…", "category": "Cement", "unit": "bag",
      "quantity": 420, "reorder_point": 100, "status": "available",
      "quantity_in_transit": 120, "last_delivery_at": "2026-11-02T09:31:00Z" }
  ],
  "meta": { "total": 20000, "limit": 20, "offset": 0 }
}
```

Status thresholds: `0` → `out_of_stock`; above `0` and at or below the reorder
point → `low_stock`; above the reorder point → `available`. A material with no
reorder point set is `available` as soon as it has stock.

`quantity_in_transit` is the total on deliveries that have left this store and
have not been resolved yet.

### `POST /company/store/stock`

The only way new material enters SiteTrack.

```json
{ "material_id": "…", "quantity": 500 }
```

`200` with the updated stock row. Adds to the current quantity.

### `PATCH /company/store/stock/{materialID}`

```json
{ "reorder_point": 100 }
```

`200` with the updated row; send `null` to clear it.

### `GET /company/deliveries`

Query: `project` (PRJ code), `direction`, `status`, `from`, `to`, `limit`, `offset`.
Every delivery in the company, newest first, same row shape as the project list.

### `POST /company/deliveries`

Sends material **from the company store to a project**.

```json
{ "project_id": "PRJ-0042", "lines": [ { "material_id": "…", "quantity": 120 } ] }
```

`201`.

### `POST /company/deliveries/{id}/record` · `.../reject` · `.../cancel`

Recording and rejecting are for deliveries coming **back** to the company store;
cancelling is for outbound ones. Same rules and status codes as the project side.
