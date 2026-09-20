#!/usr/bin/env bash
# A second copy of ForgeSync's database, on the second machine.
#
# What this gives you is not automatic failover. It is that the data is
# already on the other machine, continuously, instead of in last night's
# dump: losing the first machine stops replication until somebody promotes
# the second, and loses nothing. Promotion is one command and a few
# seconds, and the controllers find the new primary by themselves, because
# database.url names both servers and a standby refuses a connection that
# wants to write.
#
# Automatic promotion is deliberately not here. With two machines nothing
# can tell "the other one is dead" from "I cannot reach the other one",
# and a pair that promotes on its own judgement ends up, in a network
# partition, with two primaries and two databases that will not merge
# afterwards. Section 12 of README.md has the arrangement that does
# promote by itself, which takes three machines.
#
#   On the first machine:   ./standby.sh prepare <address of the second>
#   On the second machine:  ./standby.sh create  /root/forgesync-standby.txt
#   Either machine:         ./standby.sh status
#   When the first is gone: ./standby.sh promote      (on the second)
#
# Written for the Debian 13 install that deploy/prod/install.sh makes.
set -Eeuo pipefail

ETC=/etc/forgesync
SECRETS=$ETC/secrets
BUNDLE_DEFAULT=/root/forgesync-standby.txt
REPL_USER=forgesync_repl
SLOT=forgesync_standby

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
note() { printf '        %s\n' "$*"; }
warn() { printf '  \033[33mnote\033[0m  %s\n' "$*"; }
die()  { printf '  \033[31mstop\033[0m  %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run this as root"

if command -v runuser >/dev/null 2>&1; then
  as_postgres() { runuser -u postgres -- "$@"; }
elif command -v sudo >/dev/null 2>&1; then
  as_postgres() { sudo -u postgres -- "$@"; }
else
  die "neither runuser nor sudo is here"
fi

# Debian runs the cluster under systemd, and pg_ctlcluster refuses to
# start or stop one that systemd owns. Go through systemd when it is
# there, and fall back for anything that is not running that way.
pg_unit() { printf 'postgresql@%s-main' "$PGVER"; }
pg_do() { # pg_do start|stop|restart|reload
  systemctl "$1" "$(pg_unit)" 2>/dev/null && return 0
  systemctl "$1" postgresql 2>/dev/null && return 0
  as_postgres pg_ctlcluster "$PGVER" main "$1"
}

# Does the local PostgreSQL take a connection on this address, rather
# than only on the loopback? pg_isready asks exactly that and nothing
# more: logging in would also need a pg_hba entry for whoever asked, and
# being turned away by pg_hba still means it was listening.
listening_on() {
  pg_isready -h "$1" -p "$PGPORT" >/dev/null 2>&1
}

# A machine that joined as a controller has no PostgreSQL at all, so
# every one of these can legitimately be empty here. pg_lsclusters not
# existing must not end the script: pipefail would make the assignment
# fail, and errexit would take the whole thing down before the code that
# installs PostgreSQL ever runs.
cluster_field() { pg_lsclusters -h 2>/dev/null | awk -v f="$1" 'NR==1 {print $f; exit}' || true; }
read_cluster() {
  PGVER=$(cluster_field 1) || true
  PGPORT=$(cluster_field 3) || true
  PGDATA=$(cluster_field 6) || true
  CONF=/etc/postgresql/$PGVER/main
}
PGVER=""; PGPORT=""; PGDATA=""; CONF=""
read_cluster

in_recovery() { as_postgres psql -qtAX -p "$PGPORT" -c 'SELECT pg_is_in_recovery()' 2>/dev/null | tr -d ' '; }

# ------------------------------------------------------------- prepare

prepare() {
  local standby=${1:?usage: standby.sh prepare <address of the second machine>}
  [ -n "$PGVER" ] || die "no PostgreSQL cluster here; this runs on the machine that has the database"
  [ "$(in_recovery)" = f ] || die "this machine's database is a standby, not the primary"
  step "Preparing $HOSTNAME to feed a standby on $standby"

  # A role that may only stream, and a slot so the primary keeps the WAL a
  # disconnected standby still needs instead of discarding it.
  local pw
  if as_postgres psql -qtAX -p "$PGPORT" -c "SELECT 1 FROM pg_roles WHERE rolname = '$REPL_USER'" | grep -q 1; then
    ok "replication role $REPL_USER exists"
    [ -s "$SECRETS/replication.pw" ] || die "the role exists but $SECRETS/replication.pw does not; drop the role and run this again"
    pw=$(cat "$SECRETS/replication.pw")
  else
    pw=$(openssl rand -hex 24)
    printf "CREATE ROLE %s WITH REPLICATION LOGIN PASSWORD '%s'\n" "$REPL_USER" "$pw" \
      | as_postgres psql -q -p "$PGPORT" -f - >/dev/null
    umask 077; printf '%s' "$pw" > "$SECRETS/replication.pw"
    chown root:forgesync "$SECRETS/replication.pw"; chmod 0640 "$SECRETS/replication.pw"
    ok "created the replication role"
  fi
  if as_postgres psql -qtAX -p "$PGPORT" -c "SELECT 1 FROM pg_replication_slots WHERE slot_name = '$SLOT'" | grep -q 1; then
    ok "replication slot $SLOT exists"
  else
    as_postgres psql -qtAX -p "$PGPORT" -c "SELECT pg_create_physical_replication_slot('$SLOT')" >/dev/null
    ok "created the replication slot, so WAL is kept for a standby that is away"
  fi

  # Listen off the loopback, and let the standby and the other controller
  # in. listen_addresses only takes effect on a restart, unlike pg_hba,
  # which is why this restarts rather than reloads when it changed it.
  open_to_network "$standby"

  point_url_at_both "$(this_address)" "$standby"

  umask 077
  {
    printf 'FORGESYNC-STANDBY-1 %s %s %s\n' "$(this_address)" "$PGPORT" "$REPL_USER"
    printf '%s\n' "$pw"
  } > "$BUNDLE_DEFAULT"
  chmod 0600 "$BUNDLE_DEFAULT"

  cat <<DONE

  $(printf '\033[32mReady.\033[0m') $BUNDLE_DEFAULT holds the replication password.

    scp $BUNDLE_DEFAULT root@$standby:/root/
    ssh root@$standby '/usr/local/src/forgesync/deploy/prod/standby.sh create /root/$(basename "$BUNDLE_DEFAULT")'

  Delete it from both machines afterwards.

DONE
}

# This machine's address on the network the other one reaches it by.
# iproute2 is not always installed, so fall back to hostname -I, and say
# so rather than carrying on with an empty host in a connection string.
this_address() {
  local a
  # || true because pipefail turns "ip is not installed" into a failed
  # assignment, and errexit would end the script before the fallback.
  a=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}') || true
  [ -n "$a" ] || a=$(hostname -I 2>/dev/null | awk '{print $1}') || true
  [ -n "$a" ] || die "could not work out this machine's address; pass it as FORGESYNC_ADDRESS"
  printf '%s' "${FORGESYNC_ADDRESS:-$a}"
}

# Let this cluster be reached by both machines. pg_basebackup copies the
# data directory, and on Debian the configuration is not in it: a standby
# therefore keeps its own postgresql.conf and pg_hba.conf, still set to
# localhost only. That is invisible until the standby is promoted and
# nothing can reach it, so both sides do this.
open_to_network() { # open_to_network <the other machine's address>
  local other=$1 me
  me=$(this_address)
  if grep -qE "^\s*listen_addresses\s*=\s*'\*'" "$CONF/postgresql.conf"; then
    ok "listen_addresses is set in the config"
  else
    printf "\n# ForgeSync: the other machine's controller and PostgreSQL connect over the network.\nlisten_addresses = '*'\n" >> "$CONF/postgresql.conf"
    ok "set listen_addresses"
  fi
  add_hba "host replication $REPL_USER $other/32 scram-sha-256" "streaming to or from $other"
  add_hba "host forgesync forgesync $other/32 scram-sha-256" "the controller on $other"
  add_hba "host forgesync forgesync $me/32 scram-sha-256" "the controller here, which reaches it by address"
  pg_do reload
  ok "reloaded PostgreSQL, which is enough for pg_hba"

  # listen_addresses is not something PostgreSQL can reload, and a line in
  # the config it has never read is worth nothing. What settles it is
  # whether it actually answers on the address the URL names, so ask, and
  # restart if it does not. Checking the running server rather than the
  # file also fixes an earlier run that wrote the line and never restarted.
  if listening_on "$me"; then
    ok "it answers on $me:$PGPORT"
  else
    note "restarting PostgreSQL: listen_addresses only takes effect on a restart"
    pg_do restart
    local i=0
    until listening_on "$me"; do
      i=$((i + 1))
      [ "$i" -ge 20 ] && die "PostgreSQL still is not answering on $me:$PGPORT; check listen_addresses and pg_hba"
      sleep 1
    done
    ok "restarted, and it answers on $me:$PGPORT"
  fi
}

add_hba() { # add_hba <line> <why>
  if grep -qF "$1" "$CONF/pg_hba.conf"; then
    ok "pg_hba already allows $2"
  else
    printf '\n# ForgeSync: %s\n%s\n' "$2" "$1" >> "$CONF/pg_hba.conf"
    ok "pg_hba now allows $2"
  fi
}

# Both servers in database.url, so whichever is the primary is the one the
# controllers use, and a promotion needs nothing changed here.
point_url_at_both() { # point_url_at_both <primary-host> <standby-host>
  [ -n "${1:-}" ] && [ -n "${2:-}" ] \
    || die "both machines' addresses are needed to write database.url; got '${1:-}' and '${2:-}'"
  local url rest
  url=$(cat "$SECRETS/database.url")
  rest=${url#*@}                      # host:port/db?args
  local creds=${url%@*}               # postgres://user:pw
  local dbpart=${rest#*/}             # db?args
  local port=${rest%%/*}; port=${port##*:}
  dbpart=${dbpart%%\?*}
  printf '%s@%s:%s,%s:%s/%s?sslmode=disable&target_session_attrs=read-write' \
    "$creds" "$1" "$port" "$2" "$port" "$dbpart" > "$SECRETS/database.url"
  chown root:forgesync "$SECRETS/database.url"; chmod 0640 "$SECRETS/database.url"
  ok "database.url now names both servers, so a promotion needs no change here"
  systemctl restart forgesyncd 2>/dev/null && ok "restarted forgesyncd" || true
}

# -------------------------------------------------------------- create

create() {
  local bundle=${1:-$BUNDLE_DEFAULT}
  [ -s "$bundle" ] || die "$bundle is not there; run 'standby.sh prepare' on the first machine"
  local header primary port repl pw
  header=$(head -1 "$bundle")
  case "$header" in FORGESYNC-STANDBY-1\ *) ;; *) die "$bundle is not a standby bundle" ;; esac
  # shellcheck disable=SC2086 # three known fields
  set -- $header
  primary=$2; port=$3; repl=$4
  pw=$(sed -n '2p' "$bundle")
  [ -n "$primary" ] && [ -n "$pw" ] || die "$bundle is incomplete"
  chmod 0600 "$bundle"

  step "Making this machine a standby of $primary"
  if ! command -v pg_basebackup >/dev/null 2>&1; then
    note "installing PostgreSQL, which a joining machine does not get"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql >/dev/null
    read_cluster
    ok "installed PostgreSQL $PGVER"
  fi
  [ -n "$PGVER" ] || die "no PostgreSQL cluster to replace"

  if [ "$(in_recovery)" = t ]; then
    ok "this machine is already a standby"
    status
    return
  fi

  # The cluster made at install has nothing in it worth keeping: the
  # controllers have been using the first machine's database all along.
  pg_do stop 2>/dev/null || true
  local keep
  keep="$PGDATA.replaced-$(date +%Y%m%d%H%M%S)"
  mv "$PGDATA" "$keep"
  ok "moved the empty local cluster aside ($keep)"
  install -d -o postgres -g postgres -m 0700 "$PGDATA"

  PGPASSWORD=$pw as_postgres env PGPASSWORD="$pw" pg_basebackup \
    -h "$primary" -p "$port" -U "$repl" -D "$PGDATA" \
    -X stream -S "$SLOT" -R -c fast \
    || { rm -rf "$PGDATA"; mv "$keep" "$PGDATA"; die "pg_basebackup failed; the old cluster is back in place"; }
  ok "copied the database from $primary"

  pg_do start
  local i=0
  until [ "$(in_recovery)" = t ]; do
    i=$((i + 1)); [ "$i" -ge 30 ] && die "it started but is not in recovery; check the log in /var/log/postgresql"
    sleep 1
  done
  ok "started, and it is streaming from $primary"
  rm -rf "$keep"

  # The standby needs the same openness as the primary, because when it is
  # promoted it becomes the one everything connects to, and the
  # configuration did not come across with the data.
  open_to_network "$primary"

  point_url_at_both "$primary" "$(this_address)"
  status
}

# -------------------------------------------------------------- status

status() {
  step "Replication"
  [ -n "$PGVER" ] || die "no PostgreSQL cluster here"
  if [ "$(in_recovery)" = t ]; then
    ok "this machine is a STANDBY"
    as_postgres psql -qtAX -p "$PGPORT" -c "
      SELECT '  streaming from ' || coalesce(sender_host::text, '(nobody)')
          || ', ' || coalesce(status, 'stopped')
          || ', behind by ' || coalesce(round(extract(epoch from (now() - pg_last_xact_replay_timestamp())))::text, '?') || 's'
      FROM pg_stat_wal_receiver
      UNION ALL SELECT '  no WAL receiver: it is not streaming'
      WHERE NOT EXISTS (SELECT 1 FROM pg_stat_wal_receiver)"
  else
    ok "this machine is the PRIMARY"
    as_postgres psql -qtAX -p "$PGPORT" -c "
      SELECT '  standby ' || client_addr || ': ' || state
          || ', behind by ' || coalesce(pg_wal_lsn_diff(sent_lsn, replay_lsn)::text, '?') || ' bytes'
      FROM pg_stat_replication
      UNION ALL SELECT '  no standby is connected'
      WHERE NOT EXISTS (SELECT 1 FROM pg_stat_replication)"
  fi
}

# ------------------------------------------------------------- promote

promote() {
  [ -n "$PGVER" ] || die "no PostgreSQL cluster here"
  [ "$(in_recovery)" = t ] || die "this machine is already the primary; there is nothing to promote"

  cat <<WARN

  $(printf '\033[33mBefore you do this.\033[0m')

  Promoting makes this machine's database the one that takes writes. If
  the other machine's database is still running and reachable by anything,
  you will have two primaries and two histories that cannot be merged
  afterwards. Make sure the first machine is really gone, or stop its
  PostgreSQL first.

  Afterwards the old machine must not come back as a primary. Rebuild it
  as a standby of this one: run 'standby.sh prepare' here and
  'standby.sh create' there.

WARN
  if [ -t 0 ]; then
    printf '  Type "promote" to go ahead: '
    read -r answer
    [ "$answer" = promote ] || die "nothing was done"
  else
    note "not a terminal, so taking the command as the confirmation"
  fi

  step "Promoting"
  # pg_promote() asks the running server, so it needs no process control
  # and works whoever is managing the service.
  as_postgres psql -qtAX -p "$PGPORT" -c 'SELECT pg_promote(wait => true)' >/dev/null
  local i=0
  until [ "$(in_recovery)" = f ]; do
    i=$((i + 1)); [ "$i" -ge 60 ] && die "it did not leave recovery; check /var/log/postgresql"
    sleep 1
  done
  ok "this machine's database now takes writes"
  systemctl restart forgesyncd 2>/dev/null && ok "restarted forgesyncd so it reconnects at once" || true
  local j=0
  until curl -fsS -m 3 http://127.0.0.1:8090/readyz >/dev/null 2>&1; do
    j=$((j + 1)); [ "$j" -ge 30 ] && { warn "the controller here is not ready yet; 'journalctl -u forgesyncd' says why"; break; }
    sleep 1
  done
  [ "$j" -lt 30 ] && ok "the controller is ready again"
  status
}

case "${1:-}" in
  prepare) shift; prepare "$@" ;;
  create)  shift; create "$@" ;;
  status)  shift; status ;;
  promote) shift; promote ;;
  *) sed -n '2,30p' "$0"; exit 2 ;;
esac
