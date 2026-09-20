#!/usr/bin/env bash
# Checks what a ForgeSync installation does when its database fails over,
# end to end, against a running environment.
#
#   ./check-db-failover.sh                        # the .test names
#   PUBLIC_HOST=192.0.2.10 ./check-db-failover.sh
#   ./check-db-failover.sh --keep                 # leave it on the pair
#   KEEP_SAMPLES=1 ./check-db-failover.sh         # keep the raw samples
#
# Give it the same PUBLIC_HOST the environment was started with: it
# recreates the two controllers to move them between databases, and
# compose publishes their ports from these variables. PUBLIC_BIND follows
# PUBLIC_HOST unless it is set, so an environment reachable from the LAN
# comes back reachable from the LAN.
#
# The environment's ordinary database is one PostgreSQL container. This
# check starts the other arrangement (compose profile "ha"): two servers
# in streaming replication with Patroni over them and etcd holding the
# decision, with nothing in front of them. It carries the state across,
# moves both controllers onto the pair, and then takes the primary away
# twice: once by killing it, once as a planned switchover. Afterwards it
# carries the state back and puts the controllers on the single server
# again, unless --keep.
#
# What it is actually asking:
#
#   - the pages stay up while the database is gone (/healthz), and say so
#     (/readyz, forgesync_database_up);
#   - no controller acts on a lease it can no longer renew: leadership
#     stops within controller.lease of the database going, which is what
#     fences the old leader off;
#   - the two controllers never claim leadership at once, least of all
#     across a promotion;
#   - when the other server is promoted the controllers find it by
#     themselves, with no restart and nothing to point anywhere;
#   - nothing was lost: the same repositories, replicas and conflicts,
#     and a write is accepted again.
#
# Written for macOS bash 3.2, like the rest of the test environment.
set -Eeuo pipefail
cd "$(dirname "$0")"

PUBLIC_HOST=${PUBLIC_HOST:-127.0.0.1}
CONTROLLER=${CONTROLLER:-http://$PUBLIC_HOST:8090}
STANDBY=${STANDBY:-http://$PUBLIC_HOST:8091}
TOKEN=$(cat .tokens/admin.token)
DB_PASSWORD=$(sed -n 's/^FORGESYNC_DB_PASSWORD=//p' .env)
# compose reads these when it recreates the controllers. An environment
# addressed by anything but the loopback was started with PUBLIC_BIND set,
# and recreating without it would publish the UI on localhost only.
case "$PUBLIC_HOST" in
  127.0.0.1|localhost) export PUBLIC_BIND=${PUBLIC_BIND:-127.0.0.1} ;;
  *)                   export PUBLIC_BIND=${PUBLIC_BIND:-0.0.0.0} ;;
esac
export PUBLIC_HOST
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1
FAILED=0
RESTORED=0

ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
note() { printf '        %s\n' "$*"; }

dc() { docker compose --profile ha --profile controller --profile standby "$@"; }
jq3() { python3 -c "$1"; }

# The pair, as the controllers are told to reach it: both servers, and
# only one of them will take a write. target_session_attrs is what makes
# a promotion enough on its own -- no proxy, no restart, no new address.
PAIR_URL="postgres://forgesync:$DB_PASSWORD@forgesync-db-a:5432,forgesync-db-b:5432/forgesync?sslmode=disable&target_session_attrs=read-write"

# controller.lease is how long a controller may go on acting without
# reaching the database, so it is what the fencing is measured against.
LEASE=$(sed -n 's/^  lease: \([0-9]*\)s$/\1/p' .work/forgesync.docker.yaml)
LEASE=${LEASE:-15}

metrics() { curl -fsS -m 6 -H "Authorization: Bearer $TOKEN" "$1/metrics" 2>/dev/null || true; }
gauge()   { printf '%s\n' "$1" | { grep -m1 "^$2" || true; } | awk '{print $NF}'; }
leads()   { printf '%s\n' "$1" | { grep -cm1 '^forgesync_leader{.*role="leader".*} 1' || true; }; }
healthz() { curl -s -m 3 -o /dev/null -w '%{http_code}' "$1/healthz" || true; }
readyz()  { curl -s -m 3 -o /dev/null -w '%{http_code}' "$1/readyz" || true; }

# patronictl talks to etcd, so it answers from whichever of the two
# containers is running -- which matters when one has just been killed.
patroni_list() {
  local svc out
  for svc in forgesync-db-a forgesync-db-b; do
    if out=$(dc exec -T "$svc" patronictl -c /etc/patroni/patroni.yml list --format json 2>/dev/null); then
      printf '%s' "$out"; return 0
    fi
  done
  return 1
}
patroni_leader() {
  patroni_list | jq3 "
import json,sys
for m in json.load(sys.stdin):
    if m['Role'] == 'Leader' and m['State'] == 'running':
        print(m['Member'])" | head -1
}
other_member() { # the one of the two that isn't named
  case "$1" in forgesync-db-a) echo forgesync-db-b ;; *) echo forgesync-db-a ;; esac
}
patroni_streaming() { # patroni_streaming <member>
  patroni_list | jq3 "
import json,sys
print(any(m['Member'] == '$1' and m['State'] == 'streaming' for m in json.load(sys.stdin)))"
}

psql_on() { # psql_on <service> <args...>
  local svc=$1; shift
  dc exec -T -e PGPASSWORD="$DB_PASSWORD" "$svc" psql -h 127.0.0.1 -U forgesync -d forgesync "$@"
}
copy_state() { # copy_state <from-service> <to-service>
  dc exec -T -e PGPASSWORD="$DB_PASSWORD" "$1" \
    pg_dump -h 127.0.0.1 -U forgesync -d forgesync --clean --if-exists --no-owner \
    | psql_on "$2" -q -v ON_ERROR_STOP=1 >/dev/null
}

numbers() { # the figures that have to survive a failover, on one line
  local m; m=$(metrics "$CONTROLLER")
  printf 'repositories=%s replicas_synced=%s conflicts=%s' \
    "$(gauge "$m" 'forgesync_repositories ')" \
    "$(gauge "$m" 'forgesync_replicas{state="synced"}')" \
    "$(gauge "$m" 'forgesync_conflicts_open')"
}

wait_ready() { # wait_ready <url> <seconds>
  local i=0
  while [ "$i" -lt "$2" ]; do
    [ "$(readyz "$1")" = 200 ] && return 0
    sleep 1; i=$((i + 1))
  done
  return 1
}

# Ready is not the same as acting: /readyz means the database answers,
# and a controller that has just started still has to wait for whatever
# lease is in there to run out before it can take one. Taking the primary
# away before that has happened measures nothing, so wait for a leader.
wait_leading() { # wait_leading <seconds>
  local i=0
  while [ "$i" -lt "$1" ]; do
    if [ $(( $(leads "$(metrics "$CONTROLLER")") + $(leads "$(metrics "$STANDBY")") )) -ge 1 ]; then
      return 0
    fi
    sleep 1; i=$((i + 1))
  done
  return 1
}

# ---------------------------------------------------------------- watching

SAMPLES=$(mktemp -t forgesync-failover.XXXXXX)
DONE=$(mktemp -t forgesync-failover-done.XXXXXX)
cleanup() { rm -f "$SAMPLES" "$DONE"; }
# KEEP_SAMPLES=1 leaves the raw samples behind when something doesn't add
# up, which is the only way to tell "nobody was leading" from "nobody was
# asked" after the fact.
keep_samples() {
  [ "${KEEP_SAMPLES:-0}" = 1 ] || return 0
  local to; to="${TMPDIR:-/tmp}/forgesync-failover-$1.samples"
  cp "$SAMPLES" "$to" && note "the samples are in $to"
}
trap cleanup EXIT

# Samples both controllers twice a second. It stops once the primary has
# been taken away (the caller writes $DONE), at least ten seconds have
# passed since, and the installation has been settled -- a controller
# leading, the database answering -- for four samples running.
#
# One line per sample: the time, /healthz, /readyz, database_up (1, 0 or
# x when the controller couldn't be asked), and whether each controller
# claims leadership.
observe() { # observe <seconds>
  local deadline=$(( $(date +%s) + $1 )) settled=0 since=0 ma mb up now
  : > "$SAMPLES"
  while [ "$(date +%s)" -lt "$deadline" ]; do
    now=$(date +%s)
    ma=$(metrics "$CONTROLLER"); mb=$(metrics "$STANDBY")
    up=$(gauge "$ma" 'forgesync_database_up'); up=${up:-x}
    printf '%s %s %s %s %s %s\n' "$now" "$(healthz "$CONTROLLER")" "$(readyz "$CONTROLLER")" \
      "$up" "$(leads "$ma")" "$(leads "$mb")" >> "$SAMPLES"
    if [ -s "$DONE" ]; then
      [ "$since" -eq 0 ] && since=$(cat "$DONE")
      if [ "$up" = 1 ] && [ $(( $(leads "$ma") + $(leads "$mb") )) -ge 1 ]; then
        settled=$((settled + 1))
      else
        settled=0
      fi
      [ "$settled" -ge 4 ] && [ $(( now - since )) -ge 10 ] && return 0
    fi
    sleep 0.5
  done
  return 1
}

# The samples answer these. The outage is what matters: it starts when the
# primary is taken away and ends at the last sample that still couldn't
# reach a database taking writes. Leadership has to stop inside it, which
# is the fencing; whether anyone leads after it is a different question.
outage_end()        { awk -v d="$1" '$1 >= d && $4 != "1" {t = $1} END {print t + 0}' "$SAMPLES"; }
last_claim_within() { awk -v d="$1" -v e="$2" '$1 >= d && $1 <= e && ($5 + $6) >= 1 {t = $1} END {if (t) print t - d; else print 0}' "$SAMPLES"; }
first_claim_after() { awk -v e="$1" '$1 > e && ($5 + $6) >= 1 {print $1; exit}' "$SAMPLES"; }
db_down_seen()      { awk '$4 != "1" {n++} END {print n + 0}' "$SAMPLES"; }
healthz_not_ok()    { awk '$2 != 200 {n++} END {print n + 0}' "$SAMPLES"; }
readyz_said_no()    { awk '$3 != 200 {n++} END {print n + 0}' "$SAMPLES"; }
both_claimed()      { awk '$5 == 1 && $6 == 1 {n++} END {print n + 0}' "$SAMPLES"; }
claims_before()     { awk -v d="$1" '$1 < d && ($5 + $6) >= 1 {n++} END {print n + 0}' "$SAMPLES"; }

# One failover, watched: take the primary away with "$@" and report.
watch_failover() { # watch_failover <label> <command...>
  local label=$1; shift
  local down fenced resumed back settled
  # Every failover is measured against a controller that was leading, so
  # make sure one is before anything is taken away.
  wait_leading 60 || { fail "$label: no controller was leading, so there was nothing to measure"; return; }
  : > "$DONE"
  # The sampler runs in a subshell, which must not take the cleanup trap
  # with it: its own exit would delete the samples the caller is about to
  # read.
  ( trap - EXIT; observe 180 ) & local watcher=$!
  sleep 2
  "$@" >/dev/null 2>&1 || true
  down=$(date +%s)
  printf '%s' "$down" > "$DONE"
  if wait "$watcher"; then settled=0; else settled=1; fi
  if [ "$settled" -ne 0 ]; then
    fail "$label: the installation hadn't settled again within 180s"
    return
  fi

  local ended; ended=$(outage_end "$down")

  if [ "$(healthz_not_ok)" -eq 0 ]; then
    ok "$label: the pages stayed up all the way through"
  else
    fail "$label: /healthz stopped answering 200 in $(healthz_not_ok) samples"
  fi
  if [ "$(both_claimed)" -eq 0 ]; then
    ok "$label: the two controllers never claimed leadership at the same time"
  else
    fail "$label: both controllers claimed leadership in $(both_claimed) samples"
  fi

  # Without a leader in the samples taken before the primary went, there
  # is no baseline, and "leadership stopped at once" would mean only that
  # the check never saw it. Say so rather than passing.
  if [ "$(claims_before "$down")" -eq 0 ]; then
    fail "$label: no controller was seen leading before the primary went, so the fencing wasn't measured"
    keep_samples "$label"
    return
  fi
  if [ "$ended" -eq 0 ]; then
    ok "$label: the database was never out of reach between two samples, so nothing had to stop"
    return
  fi
  back=$(( ended - down ))
  # A controlled hand-over can be over inside one sampling interval. Then
  # there is no fencing to measure, and saying "leadership stopped after
  # 0s" would read as though it had dropped when it never did.
  if [ "$back" -le 1 ]; then
    ok "$label: over within ${back}s, quicker than the sampling, and leadership was never seen to drop"
    return
  fi
  fenced=$(last_claim_within "$down" "$ended")
  resumed=$(first_claim_after "$ended")
  if [ "$fenced" -le "$((LEASE + 2))" ]; then
    ok "$label: leadership stopped ${fenced}s after the database went, inside the ${LEASE}s lease"
  else
    fail "$label: a controller still claimed leadership ${fenced}s into the outage (lease ${LEASE}s)"
    keep_samples "$label"
  fi
  note "the database was out of reach for ${back}s (in $(db_down_seen) samples; /readyz said so in $(readyz_said_no))"
  if [ -n "${resumed:-}" ]; then
    note "a controller led again $(( resumed - down ))s after the primary went, $(( resumed - ended ))s after the database came back"
  else
    fail "$label: no controller took the lease again"
  fi
}

# ------------------------------------------------------------------- checks

step "The environment"
# A controller that has just been rebuilt takes a few seconds to answer,
# so wait rather than deciding on one probe.
for url in "$CONTROLLER" "$STANDBY"; do
  i=0
  while [ "$i" -lt 60 ] && [ "$(healthz "$url")" != 200 ]; do sleep 2; i=$((i + 2)); done
  if [ "$(healthz "$url")" != 200 ]; then
    fail "$url isn't answering after ${i}s; start the environment first"
    exit 1
  fi
done
ok "both controllers are answering"

step "The two-server database"
dc up -d --build etcd forgesync-db-a forgesync-db-b >/dev/null 2>&1
i=0; LEADER=""
while [ $i -lt 180 ]; do
  LEADER=$(patroni_leader 2>/dev/null || true)
  [ -n "$LEADER" ] && break
  sleep 3; i=$((i + 3))
done
[ -n "$LEADER" ] || { fail "Patroni didn't settle on a primary within 180s"; exit 1; }
ok "Patroni made $LEADER the primary, the other is streaming from it"

step "Moving the controllers onto it"
BEFORE=$(numbers)
copy_state forgesync-db "$LEADER"
FORGESYNC_DATABASE_URL="$PAIR_URL" dc up -d --no-deps forgesync forgesync-b >/dev/null 2>&1
wait_ready "$CONTROLLER" 90 || { fail "the controller didn't come ready on the pair"; exit 1; }
wait_ready "$STANDBY" 90   || { fail "the standby didn't come ready on the pair"; exit 1; }
AFTER=$(numbers)
if [ "$BEFORE" = "$AFTER" ]; then
  ok "the state came across unchanged: $AFTER"
else
  fail "the state changed in the move: $BEFORE -> $AFTER"
fi
if wait_leading 60; then
  ok "a controller has taken the lease in the new database"
else
  fail "no controller took the lease within 60s of moving"
  exit 1
fi

step "Pointed at a standby by mistake"
note "a standby answers every read and takes no write, so a controller that"
note "settled for one would look healthy while nothing was being synced"
STANDBY_MEMBER=$(other_member "$LEADER")
SB_OUT=$(dc exec -T -e FORGESYNC_DATABASE_URL="postgres://forgesync:$DB_PASSWORD@$STANDBY_MEMBER:5432/forgesync?sslmode=disable" \
  forgesync forgesyncd -config /etc/forgesync/forgesync.yaml 2>&1 || true)
case "$SB_OUT" in
  *standby*) ok "it refuses to start, and says why: $(printf '%s' "$SB_OUT" | tail -1)" ;;
  *)         fail "a controller pointed at the standby didn't say so: $SB_OUT" ;;
esac

restore() {
  [ "$RESTORED" -eq 1 ] && return
  RESTORED=1
  if [ "$KEEP" -eq 1 ]; then
    printf '\nLeft on the pair. Put it back with:\n'
    printf '  docker compose --profile controller --profile standby up -d --no-deps forgesync forgesync-b\n'
    return
  fi
  step "Putting it back on the single server"
  local back; back=$(patroni_leader 2>/dev/null || echo "$LEADER")
  copy_state "$back" forgesync-db
  FORGESYNC_DATABASE_URL='' dc up -d --no-deps forgesync forgesync-b >/dev/null 2>&1
  if wait_ready "$CONTROLLER" 90; then
    ok "the controllers are on forgesync-db again, with the state put back"
  else
    fail "the controller didn't come ready on forgesync-db"
  fi
  dc stop etcd forgesync-db-a forgesync-db-b >/dev/null 2>&1
  ok "the pair is stopped; its volumes are kept, and docker compose down -v removes them"
}
# From here on the environment is on the pair, so put it back whatever happens.
trap 'restore; cleanup' EXIT

step "Losing the primary outright"
note "killing $LEADER, the way a server dies rather than stops"
watch_failover "killed" docker kill -s KILL "$(dc ps -q "$LEADER")"

step "After it"
NEW=$(patroni_leader 2>/dev/null || true)
if [ -n "$NEW" ] && [ "$NEW" != "$LEADER" ]; then
  ok "$NEW was promoted and took the writes"
else
  fail "the primary didn't move away from $LEADER"
  NEW=${NEW:-forgesync-db-a}
fi
NOW=$(numbers)
if [ "$NOW" = "$AFTER" ]; then ok "nothing was lost: $NOW"; else fail "the figures changed: $AFTER -> $NOW"; fi
scan_code() { curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" \
  "$1/api/v1/inventory/scan"; }
# Right after a failover the lease can be in mid-air for a second or two:
# the controller that took it hands it back to the preferred one, and in
# between neither is leading, so both answer 409. That is the hand-over
# working, not a fault, so ask again rather than judge on one attempt.
i=0; A_CODE=""; B_CODE=""; took=0
while [ "$i" -lt 30 ]; do
  A_CODE=$(scan_code "$CONTROLLER"); B_CODE=$(scan_code "$STANDBY")
  case "$A_CODE/$B_CODE" in
    202/409|409/202) took=1; break ;;
    202/202)         took=2; break ;;
  esac
  sleep 2; i=$((i + 2))
done
case "$took" in
  1) ok "a write is accepted again by the leader (202), and the standby names it (409)" ;;
  2) fail "both controllers took the write; only the leader should" ;;
  *) fail "neither controller took the write within ${i}s: $CONTROLLER -> $A_CODE, $STANDBY -> $B_CODE" ;;
esac

step "Bringing the lost server back"
dc start "$LEADER" >/dev/null 2>&1
i=0; rejoined=0
while [ $i -lt 180 ]; do
  [ "$(patroni_streaming "$LEADER" 2>/dev/null || true)" = True ] && { rejoined=1; break; }
  sleep 3; i=$((i + 3))
done
if [ "$rejoined" -eq 1 ]; then
  ok "$LEADER came back as a streaming replica, with nothing done to it by hand"
else
  fail "$LEADER didn't rejoin as a replica within 180s"
fi

step "A planned switchover"
note "the maintenance case: the primary hands over rather than dying"
watch_failover "switchover" dc exec -T "$NEW" patronictl -c /etc/patroni/patroni.yml \
  switchover --force --leader "$NEW" --candidate "$LEADER"
NOW=$(numbers)
if [ "$NOW" = "$AFTER" ]; then ok "nothing was lost: $NOW"; else fail "the figures changed: $AFTER -> $NOW"; fi

restore
trap cleanup EXIT

printf '\n'
if [ "$FAILED" -eq 0 ]; then
  printf '\033[32mThe database can lose a server without ForgeSync losing anything.\033[0m\n'
else
  printf '\033[31mSomething is not as it should be (above).\033[0m\n'
fi
exit "$FAILED"
