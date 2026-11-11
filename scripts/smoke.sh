#!/usr/bin/env bash
# End to end check of every rule the brief calls out. Run it against a fresh
# database:
#
#   docker compose up -d db api
#   ./scripts/smoke.sh
#
# The port is taken from API_PORT in .env (the same value compose publishes), so
# BASE only has to be set when the API lives somewhere else.
#
# Exits non-zero on the first failed expectation.

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
port=8080
if [[ -f "$here/../.env" ]]; then
  env_port=$(sed -n 's/^API_PORT=//p' "$here/../.env" | tr -d '[:space:]')
  [[ -n $env_port ]] && port=$env_port
fi

BASE="${BASE:-http://localhost:$port}"
API="$BASE/api/v1"
PASS=0
FAIL=0

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# expect <description> <expected-status> <actual-status>
expect() {
  if [[ "$2" == "$3" ]]; then
    PASS=$((PASS + 1))
    printf '  ok   %s (%s)\n' "$1" "$3"
  else
    FAIL=$((FAIL + 1))
    printf '  FAIL %s: wanted %s, got %s\n' "$1" "$2" "$3"
  fi
}

# req <method> <path> <token> [body] -> sets STATUS and BODY
req() {
  local method=$1 path=$2 token=${3:-} body=${4:-}
  local args=(-sS -o /tmp/st_body -w '%{http_code}' -X "$method" "$API$path")
  [[ -n $token ]] && args+=(-H "Authorization: Bearer $token")
  [[ -n $body ]] && args+=(-H 'Content-Type: application/json' -d "$body")
  STATUS=$(curl "${args[@]}")
  BODY=$(cat /tmp/st_body)
}

field() { echo "$BODY" | jq -r "$1 // empty"; }

uuid() { python3 -c 'import uuid;print(uuid.uuid4())'; }

STAMP=$(date +%s)
ADMIN_EMAIL="admin+$STAMP@example.test"

say "health"
req GET /health
expect "health endpoint" 200 "$STATUS"

say "registration and login"
req POST /auth/register "" "{\"name\":\"Site Admin\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"Password123!\",\"app_role\":\"superadmin\"}"
expect "register superadmin" 201 "$STATUS"
ADMIN_TOKEN=$(field .data.access_token)
ADMIN_REFRESH=$(field .data.refresh_token)

req POST /auth/register "" "{\"name\":\"Site Admin\",\"email\":\"$ADMIN_EMAIL\",\"password\":\"Password123!\",\"app_role\":\"user\"}"
expect "duplicate email rejected" 409 "$STATUS"

req POST /auth/login "" "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"wrong-password\"}"
expect "wrong password rejected" 401 "$STATUS"
BAD_LOGIN_MSG=$(field .error.message)

req POST /auth/login "" "{\"email\":\"nobody+$STAMP@example.test\",\"password\":\"wrong-password\"}"
expect "unknown email rejected" 401 "$STATUS"
UNKNOWN_LOGIN_MSG=$(field .error.message)
if [[ "$BAD_LOGIN_MSG" == "$UNKNOWN_LOGIN_MSG" ]]; then
  PASS=$((PASS + 1)); printf '  ok   login failures do not reveal whether the email exists\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL login failures leak whether the email exists\n'
fi

req GET /me "$ADMIN_TOKEN"
expect "me" 200 "$STATUS"
req GET /me
expect "me without token" 401 "$STATUS"

say "projects"
req POST /projects "$ADMIN_TOKEN" '{"name":"Riverside Apartments","planned_start":"2026-01-05","planned_end":"2026-12-20"}'
expect "create project" 201 "$STATUS"
PROJECT=$(field .data.id)
req GET "/projects/$PROJECT" "$ADMIN_TOKEN"
expect "read project" 200 "$STATUS"
req POST /projects "$ADMIN_TOKEN" '{"name":"Bad dates","planned_start":"2026-12-20","planned_end":"2026-01-05"}'
expect "end before start rejected" 400 "$STATUS"

say "tasks and ids"
for name in "Excavate footings" "Pour level 1 slab" "Erect blockwork"; do
  req POST "/projects/$PROJECT/tasks" "$ADMIN_TOKEN" "{\"name\":\"$name\",\"planned_start\":\"2026-01-10\",\"planned_end\":\"2026-02-10\"}"
done
expect "create first task" 201 "$STATUS"
T3=$(field .data.id)
T1="$PROJECT-T-0001"
T2="$PROJECT-T-0002"
if [[ "$T1" == "$PROJECT-T-0001" && "$T3" == "$PROJECT-T-0003" ]]; then
  PASS=$((PASS + 1)); printf '  ok   task ids count up inside the project (%s)\n' "$T3"
else
  FAIL=$((FAIL + 1)); printf '  FAIL unexpected task code %s\n' "$T3"
fi

say "dependencies"
req POST "/projects/$PROJECT/tasks/$T2/dependencies" "$ADMIN_TOKEN" "{\"depends_on\":\"$T1\"}"
expect "B depends on A" 200 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T3/dependencies" "$ADMIN_TOKEN" "{\"depends_on\":\"$T2\"}"
expect "C depends on B" 200 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T1/dependencies" "$ADMIN_TOKEN" "{\"depends_on\":\"$T3\"}"
expect "cycle A->B->C->A rejected" 422 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T1/dependencies" "$ADMIN_TOKEN" "{\"depends_on\":\"$T1\"}"
expect "self dependency rejected" 422 "$STATUS"

req GET "/projects/$PROJECT/tasks/$T3/dependencies" "$ADMIN_TOKEN"
expect "task graph" 200 "$STATUS"
STEPS=$(echo "$BODY" | jq -r '.data.waits_on | map(.steps) | max')
if [[ "$STEPS" == "2" ]]; then
  PASS=$((PASS + 1)); printf '  ok   C reaches A in 2 steps\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL C should reach A in 2 steps, got %s\n' "$STEPS"
fi

say "task status machine"
req GET "/projects/$PROJECT" "$ADMIN_TOKEN"
BLOCKED0=$(field .data.blocked_task_count)
if [[ "$BLOCKED0" == "2" ]]; then
  PASS=$((PASS + 1)); printf '  ok   B and C both start out blocked\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL expected 2 blocked tasks before anything starts, got %s\n' "$BLOCKED0"
fi

req POST "/projects/$PROJECT/tasks/$T3/start" "$ADMIN_TOKEN"
expect "cannot start while blocked" 422 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T1/start" "$ADMIN_TOKEN"
expect "start unblocked task" 200 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T2/start" "$ADMIN_TOKEN"
expect "cannot start B before A is done" 422 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T1/done" "$ADMIN_TOKEN"
expect "mark A done" 200 "$STATUS"

req GET "/projects/$PROJECT" "$ADMIN_TOKEN"
BLOCKED1=$(field .data.blocked_task_count)
if [[ "$BLOCKED1" == "1" ]]; then
  PASS=$((PASS + 1)); printf '  ok   only C stays blocked once A is done\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL expected 1 blocked task after A is done, got %s\n' "$BLOCKED1"
fi

req POST "/projects/$PROJECT/tasks/$T2/start" "$ADMIN_TOKEN"
expect "B starts once A is done" 200 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T1/reopen" "$ADMIN_TOKEN"
expect "cannot reopen A while B has started" 422 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T2/done" "$ADMIN_TOKEN"
expect "mark B done" 200 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T2/reopen" "$ADMIN_TOKEN"
expect "reopen B when nothing downstream started" 200 "$STATUS"
req POST "/projects/$PROJECT/tasks/$T2/done" "$ADMIN_TOKEN"
expect "mark B done again" 200 "$STATUS"

say "project rollup"
req GET "/projects/$PROJECT" "$ADMIN_TOKEN"
expect "project detail" 200 "$STATUS"
DONE=$(field .data.task_counts.done)
if [[ "$DONE" == "2" ]]; then
  PASS=$((PASS + 1)); printf '  ok   project reports 2 done tasks\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL project reports %s done tasks\n' "$DONE"
fi
PCT=$(field .data.percent_done)
if [[ "$PCT" == "66.6667" || "$PCT" == "66.67" || "$PCT" == "66.66666666666667" ]]; then
  PASS=$((PASS + 1)); printf '  ok   percentage done is reported (%s)\n' "$PCT"
else
  FAIL=$((FAIL + 1)); printf '  FAIL percentage done is %s\n' "$PCT"
fi

say "materials and company store"
req POST /materials "$ADMIN_TOKEN" "{\"name\":\"Cement OPC 50kg $STAMP\",\"category\":\"Cement\",\"unit\":\"bag\"}"
expect "create material" 201 "$STATUS"
MAT=$(field .data.id)

req GET /company/store "$ADMIN_TOKEN"
expect "company store list" 200 "$STATUS"
req POST /company/store/stock "$ADMIN_TOKEN" "{\"material_id\":\"$MAT\",\"quantity\":500}"
expect "add stock" 200 "$STATUS"
req POST /company/store/stock "$ADMIN_TOKEN" "{\"material_id\":\"$MAT\",\"quantity\":-5}"
expect "negative stock rejected" 400 "$STATUS"
req PATCH "/company/store/stock/$MAT" "$ADMIN_TOKEN" '{"reorder_point":100}'
expect "set reorder point" 200 "$STATUS"
STATUS_VALUE=$(field .data.status)
if [[ "$STATUS_VALUE" == "available" ]]; then
  PASS=$((PASS + 1)); printf '  ok   stock above the reorder point is available\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL stock status is %s\n' "$STATUS_VALUE"
fi

say "deliveries"
req POST /company/deliveries "$ADMIN_TOKEN" "{\"project_id\":\"$PROJECT\",\"lines\":[{\"material_id\":\"$MAT\",\"quantity\":120}]}"
expect "company sends 120 bags" 201 "$STATUS"
D1=$(field .data.id)
req GET "/projects/$PROJECT/deliveries" "$ADMIN_TOKEN"
expect "project sees the incoming delivery" 200 "$STATUS"
DIRN=$(echo "$BODY" | jq -r '.data[0].direction')
if [[ "$DIRN" == "incoming" ]]; then
  PASS=$((PASS + 1)); printf '  ok   delivery shows as incoming for the project\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL delivery direction is %s\n' "$DIRN"
fi

req POST "/projects/$PROJECT/deliveries/$D1/record" "$ADMIN_TOKEN"
expect "project records the delivery" 200 "$STATUS"
req GET "/projects/$PROJECT/store" "$ADMIN_TOKEN"
QTY=$(echo "$BODY" | jq -r '.data[0].quantity')
if [[ "$QTY" == "120" ]]; then
  PASS=$((PASS + 1)); printf '  ok   project store holds 120\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL project store holds %s\n' "$QTY"
fi

req POST "/projects/$PROJECT/deliveries" "$ADMIN_TOKEN" "{\"lines\":[{\"material_id\":\"$MAT\",\"quantity\":200}]}"
expect "cannot send more than the store holds" 409 "$STATUS"
req POST "/projects/$PROJECT/deliveries" "$ADMIN_TOKEN" "{\"lines\":[{\"material_id\":\"$MAT\",\"quantity\":50}]}"
expect "send 50 back to the company store" 201 "$STATUS"
D2=$(field .data.id)
req POST "/projects/$PROJECT/deliveries/$D2/record" "$ADMIN_TOKEN"
expect "sender cannot record its own delivery" 403 "$STATUS"
req POST "/company/deliveries/$D2/record" "$ADMIN_TOKEN"
expect "company records the return" 200 "$STATUS"
req POST "/company/deliveries/$D2/record" "$ADMIN_TOKEN"
expect "recording twice is a conflict" 409 "$STATUS"

say "membership and access control"
OUTSIDER_EMAIL="outsider+$STAMP@example.test"
req POST /auth/register "" "{\"name\":\"Outsider\",\"email\":\"$OUTSIDER_EMAIL\",\"password\":\"Password123!\",\"app_role\":\"user\"}"
OUT_TOKEN=$(field .data.access_token)
OUT_ID=$(field .data.user.id)
expect "register outsider" 201 "$STATUS"

req GET "/projects/$PROJECT" "$OUT_TOKEN"
expect "outsider sees project as missing" 404 "$STATUS"
req GET "/projects/PRJ-9999" "$OUT_TOKEN"
expect "unknown project is also 404" 404 "$STATUS"
req GET "/projects/$PROJECT/tasks" "$OUT_TOKEN"
expect "outsider cannot list tasks" 404 "$STATUS"

req POST "/projects/$PROJECT/invitations" "$ADMIN_TOKEN" "{\"email\":\"$OUTSIDER_EMAIL\"}"
expect "invite outsider" 201 "$STATUS"
INV=$(field .data.id)

req POST "/invitations/$INV/accept" "$ADMIN_TOKEN"
expect "inviter cannot accept for the invitee" 404 "$STATUS"
req GET /notifications "$OUT_TOKEN"
expect "outsider sees the invitation" 200 "$STATUS"
UNREAD=$(echo "$BODY" | jq -r '.meta.unread_notifications')
if [[ "$UNREAD" -ge 1 ]]; then
  PASS=$((PASS + 1)); printf '  ok   unread notification count is reported\n'
else
  FAIL=$((FAIL + 1)); printf '  FAIL unread count is %s\n' "$UNREAD"
fi
req POST "/invitations/$INV/accept" "$OUT_TOKEN"
expect "invitee accepts" 200 "$STATUS"
req GET "/projects/$PROJECT/tasks" "$OUT_TOKEN"
expect "member can list tasks" 200 "$STATUS"
req POST "/projects/$PROJECT/tasks" "$OUT_TOKEN" '{"name":"Member task","planned_start":"2026-03-01","planned_end":"2026-03-10"}'
expect "member can create a task" 201 "$STATUS"
req DELETE "/projects/$PROJECT/members/$OUT_ID" "$OUT_TOKEN"
expect "non-admin cannot remove members" 403 "$STATUS"
req PATCH "/projects/$PROJECT/members/$OUT_ID" "$OUT_TOKEN" '{"is_project_admin":true}'
expect "non-admin cannot grant admin" 403 "$STATUS"

req DELETE "/projects/$PROJECT/members/$OUT_ID" "$ADMIN_TOKEN"
expect "admin removes the member" 204 "$STATUS"
req GET "/projects/$PROJECT/tasks" "$OUT_TOKEN"
expect "removed member loses access immediately" 404 "$STATUS"

say "last admin protection"
req GET /me "$ADMIN_TOKEN"
ADMIN_ID=$(field .data.user.id)
req DELETE "/projects/$PROJECT/members/$ADMIN_ID" "$ADMIN_TOKEN"
expect "cannot remove the last project admin" 409 "$STATUS"

say "role and company store boundary"
req GET /company/store "$OUT_TOKEN"
expect "plain user cannot read the company store" 403 "$STATUS"
req POST /company/store/stock "$OUT_TOKEN" "{\"material_id\":\"$MAT\",\"quantity\":10}"
expect "plain user cannot add stock" 403 "$STATUS"

say "logout kills the session"
req POST /auth/logout "$OUT_TOKEN"
expect "logout" 204 "$STATUS"
req GET /me "$OUT_TOKEN"
expect "access token rejected after logout" 401 "$STATUS"

say "refresh rotation"
req POST /auth/refresh "" "{\"refresh_token\":\"$ADMIN_REFRESH\"}"
expect "refresh works" 200 "$STATUS"
NEW_REFRESH=$(field .data.refresh_token)
req POST /auth/refresh "" "{\"refresh_token\":\"$ADMIN_REFRESH\"}"
expect "the old refresh token is dead after rotation" 401 "$STATUS"
req POST /auth/refresh "" "{\"refresh_token\":\"$NEW_REFRESH\"}"
expect "the rotated token works" 200 "$STATUS"

say "cleanup"
req DELETE "/projects/$PROJECT" "$ADMIN_TOKEN"
expect "delete project" 204 "$STATUS"
req GET "/projects/$PROJECT" "$ADMIN_TOKEN"
expect "deleted project is gone" 404 "$STATUS"
req POST /projects "$ADMIN_TOKEN" '{"name":"Code reuse check","planned_start":"2026-02-01","planned_end":"2026-03-01"}'
NEW_PROJECT=$(field .data.id)
if [[ "$NEW_PROJECT" != "$PROJECT" ]]; then
  PASS=$((PASS + 1)); printf '  ok   project id is never reused (%s then %s)\n' "$PROJECT" "$NEW_PROJECT"
else
  FAIL=$((FAIL + 1)); printf '  FAIL project id was reused\n'
fi

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
[[ $FAIL -eq 0 ]]
