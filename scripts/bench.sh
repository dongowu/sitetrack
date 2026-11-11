#!/usr/bin/env bash
# Reproduces the timings quoted under "Measured timings" in the README.
#
#   docker compose up -d db api
#   docker compose run --rm seed --reset      # full scale, ~2m20s
#   ./scripts/bench.sh
#
# Every request runs three times and the median wall time is reported, together
# with the rows returned and the total the endpoint reports, so a fast number
# cannot come from a smaller-than-expected result set.

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
port=8080
if [[ -f "$here/../.env" ]]; then
  env_port=$(sed -n 's/^API_PORT=//p' "$here/../.env" | tr -d '[:space:]')
  [[ -n $env_port ]] && port=$env_port
fi

BASE="${BASE:-http://localhost:$port}"
API="$BASE/api/v1"
EMAIL="${SEED_EMAIL:-superadmin@sitetrack.test}"
PASSWORD="${SEED_PASSWORD:-Password123!}"

token=$(curl -sS -X POST "$API/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" | jq -r '.data.access_token // empty')
if [[ -z $token ]]; then
  echo "login failed against $API" >&2
  exit 1
fi

# The worst case for the project list is whichever project has the most late
# tasks; the worst case for the task list is whichever project has the most
# tasks. Both are picked from the API so the script needs no psql access.
project=$(curl -sS "$API/projects?sort=late_tasks&dir=desc&limit=1" \
  -H "Authorization: Bearer $token" | jq -r '.data[0].id // empty')
if [[ -z $project ]]; then
  echo "no projects found - seed the database first" >&2
  exit 1
fi
task=$(curl -sS "$API/projects/$project/tasks?limit=1" \
  -H "Authorization: Bearer $token" | jq -r '.data[0].id // empty')

printf '%-52s %9s %9s %9s\n' ENDPOINT MEDIAN ROWS TOTAL
printf '%-52s %9s %9s %9s\n' -------- ------ ---- -----

bench() {
  local label=$1 path=$2 times=() t body rows total

  for _ in 1 2 3; do
    t=$(curl -sS -o /tmp/st_bench_body -w '%{time_total}' "$API$path" \
      -H "Authorization: Bearer $token")
    times+=("$t")
  done
  body=$(cat /tmp/st_bench_body)

  # three samples: the median is the middle one after sorting
  median=$(printf '%s\n' "${times[@]}" | sort -n | sed -n '2p')
  rows=$(echo "$body" | jq -r 'if (.data | type) == "array" then (.data | length) else 1 end' 2>/dev/null || echo '?')
  total=$(echo "$body" | jq -r '.meta.total // "-"' 2>/dev/null || echo '?')

  printf '%-52s %8.0fms %9s %9s\n' "$label" "$(echo "$median * 1000" | bc)" "$rows" "$total"
}

bench "GET /projects"                                  "/projects?limit=20"
bench "GET /projects?sort=late_tasks"                  "/projects?limit=20&sort=late_tasks&dir=desc"
bench "GET /projects?offset=1000"                      "/projects?limit=20&offset=1000"
bench "GET /projects/$project"                         "/projects/$project"
bench "GET /projects/$project/tasks"                   "/projects/$project/tasks?limit=20"
bench "GET /projects/$project/tasks?blocked=true"      "/projects/$project/tasks?limit=20&blocked=true"
bench "GET /projects/$project/tasks?status=late"       "/projects/$project/tasks?limit=20&status=late"
bench "GET /projects/$project/tasks/$task"             "/projects/$project/tasks/$task"
bench "GET /projects/$project/store"                   "/projects/$project/store?limit=20"
bench "GET /projects/$project/deliveries"              "/projects/$project/deliveries?limit=20"
bench "GET /company/store"                             "/company/store?limit=20"
bench "GET /company/store?status=low_stock"            "/company/store?limit=20&status=low_stock"
bench "GET /company/deliveries"                        "/company/deliveries?limit=20"
bench "GET /materials"                                 "/materials?limit=20"
bench "GET /me"                                        "/me"
bench "GET /notifications"                             "/notifications?limit=20"

cat <<'EOF'

Read this as: median of three runs, warm, single laptop, API and Postgres both
in Docker on the same host. The company store and the project list are the two
endpoints that aggregate over the whole visible set; everything else is indexed
lookups or a single page of rows.
EOF
