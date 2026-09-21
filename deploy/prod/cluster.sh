#!/usr/bin/env bash
# The three-machine database: one that promotes itself.
#
# Two machines can keep a second copy (standby.sh) but cannot promote it
# safely, because nothing can tell "the other one is dead" from "I cannot
# reach the other one". Three can: a majority is two, so the two that can
# see each other decide and the one that cannot has no say. That is the
# same reason a Proxmox cluster wants three nodes.
#
# What this sets up is the standard arrangement, from Debian's own
# packages: etcd holding the decision, Patroni making it, PostgreSQL
# doing the work. ForgeSync itself needs no telling: database.url names
# all three and a standby refuses a connection that wants to write, so
# whichever is the primary is the one the controllers use.
#
#   On the first machine:   ./cluster.sh init <second> <third>
#   On the others:          ./cluster.sh join /root/forgesync-cluster.txt
#   Any machine:            ./cluster.sh status
#   Planned handover:       ./cluster.sh switchover
#
# init takes a verified dump before it touches anything, and refuses to
# go on if it cannot verify it. Handing a live database to another piece
# of software is the one thing here worth being frightened of.
#
# This replaces standby.sh: Patroni does the replication, the promotion
# and the rejoining, and pg_ctlcluster must not be used on a cluster it
# owns.
set -Eeuo pipefail

ETC=/etc/forgesync
SECRETS=$ETC/secrets
SCOPE=${FORGESYNC_SCOPE:-forgesync}
BUNDLE_DEFAULT=/root/forgesync-cluster.txt
PATRONI_ETC=/etc/patroni
PATRONI_CONF=/etc/patroni/config.yml
REPL_USER=forgesync_repl

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

this_address() {
  local a
  a=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}') || true
  [ -n "$a" ] || a=$(hostname -I 2>/dev/null | awk '{print $1}') || true
  [ -n "$a" ] || die "could not work out this machine's address; set FORGESYNC_ADDRESS"
  printf '%s' "${FORGESYNC_ADDRESS:-$a}"
}

cluster_field() { pg_lsclusters -h 2>/dev/null | awk -v f="$1" 'NR==1 {print $f; exit}' || true; }

# A member's id, looked up by its peer URL rather than read out of what
# "member add" printed: the id has no fixed width (a leading zero is not
# shown) and the wording is etcdctl's to change.
etcd_member_id() { # etcd_member_id <an endpoint host> <this machine>
  ETCDCTL_API=3 etcdctl --endpoints "http://$1:2379" member list 2>/dev/null \
    | awk -F', ' -v u="http://$2:2380" '$4 == u {print $1}'
}

# The last field of a member list line says whether it is a learner.
etcd_is_learner() { # etcd_is_learner <an endpoint host> <this machine>
  [ "$(ETCDCTL_API=3 etcdctl --endpoints "http://$1:2379" member list 2>/dev/null \
    | awk -F', ' -v u="http://$2:2380" '$4 == u {print $6}' | tr -d ' ')" = true ]
}

packages() {
  step "Packages"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  # python3-etcd is the one Patroni needs: its etcd3 driver builds on the
  # same base module, and without it Patroni starts, finds no usable
  # store and stops, offering only consul and kubernetes.
  apt-get install -y -qq etcd-server etcd-client patroni python3-etcd postgresql >/dev/null
  ok "etcd $(etcd --version 2>/dev/null | awk 'NR==1{print $3}'), patroni $(patroni --version 2>/dev/null | awk '{print $2}')"
  # Debian starts PostgreSQL itself. Patroni has to be the only thing
  # that starts and stops it, or the two fight over the same data. Only
  # disabled here, not stopped: the database is still wanted for a moment
  # longer, and stopping it is done deliberately below.
  systemctl disable --quiet "postgresql@$(cluster_field 1)-main" 2>/dev/null || true
  systemctl disable --quiet postgresql 2>/dev/null || true
  ok "Debian's own PostgreSQL unit will not start it at boot; Patroni owns it now"
}

write_etcd() { # write_etcd <state: new|existing> <initial-cluster>
  local me; me=$(this_address)
  cat > /etc/default/etcd <<CONF
# Written by forgesync cluster.sh. etcd holds one thing: which PostgreSQL
# is the primary. It is tiny and it must be an odd number of machines.
ETCD_NAME=$(hostname -s)
ETCD_DATA_DIR=/var/lib/etcd/default
ETCD_LISTEN_PEER_URLS=http://0.0.0.0:2380
ETCD_LISTEN_CLIENT_URLS=http://0.0.0.0:2379
ETCD_INITIAL_ADVERTISE_PEER_URLS=http://$me:2380
ETCD_ADVERTISE_CLIENT_URLS=http://$me:2379
ETCD_INITIAL_CLUSTER=$2
ETCD_INITIAL_CLUSTER_STATE=$1
ETCD_INITIAL_CLUSTER_TOKEN=$SCOPE
CONF
  chmod 0644 /etc/default/etcd
  install -d -o etcd -g etcd -m 0700 /var/lib/etcd
  systemctl enable --quiet etcd
  systemctl restart etcd
  # A learner answers reads but cannot commit, so "endpoint health",
  # which commits a proposal to prove the cluster works, always fails on
  # one. Ask a joining member for its own status instead; the promotion
  # that follows is what proves it caught up.
  local i=0 check=health
  [ "$1" = existing ] && check=status
  until etcdctl endpoint "$check" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -ge 30 ] && die "etcd did not answer on this machine; 'journalctl -u etcd' says why"
    sleep 1
  done
  ok "etcd is up on $me"
}

write_patroni() { # write_patroni <etcd-hosts> <superuser-pw> <repl-pw> <pgver> <pgdata> <pgport>
  local me; me=$(this_address)
  install -d -o postgres -g postgres -m 0750 "$PATRONI_ETC"
  cat > "$PATRONI_CONF" <<CONF
# Written by forgesync cluster.sh.
#
# Patroni decides which of the three PostgreSQL servers takes writes, and
# etcd remembers the decision. Nothing here is ForgeSync: the controllers
# name all three servers in database.url and a standby refuses a
# connection that wants to write, so a promotion needs nothing changed.
#
# Do not use pg_ctlcluster on this cluster any more. Patroni starts and
# stops PostgreSQL; going round it is how two things end up writing to
# one data directory.
scope: $SCOPE
name: $(hostname -s)

restapi:
  listen: 0.0.0.0:8008
  connect_address: $me:8008

etcd3:
  hosts: $1

bootstrap:
  dcs:
    # A leader that cannot reach etcd for ttl seconds gives up, and the
    # others elect. Keep it comfortably above loop_wait and retry_timeout.
    ttl: 30
    loop_wait: 10
    retry_timeout: 10
    maximum_lag_on_failover: 1048576
    postgresql:
      use_pg_rewind: true
      parameters:
        wal_level: replica
        hot_standby: "on"
        wal_log_hints: "on"
        max_wal_senders: 10
        max_replication_slots: 10

postgresql:
  listen: 0.0.0.0:$6
  connect_address: $me:$6
  data_dir: $5
  bin_dir: /usr/lib/postgresql/$4/bin
  pgpass: /var/lib/postgresql/.pgpass_patroni
  authentication:
    superuser:
      username: forgesync
      password: '$2'
    replication:
      username: $REPL_USER
      password: '$3'
  parameters:
    password_encryption: scram-sha-256
  pg_hba:
    - local all all peer
    - host replication $REPL_USER 0.0.0.0/0 scram-sha-256
    - host all all 0.0.0.0/0 scram-sha-256

tags:
  nofailover: false
  noloadbalance: false
  clonefrom: false
  nosync: false
CONF
  chown postgres:postgres "$PATRONI_CONF"
  chmod 0600 "$PATRONI_CONF"
  ok "wrote $PATRONI_CONF (0600, owned by postgres: it holds the passwords)"

  # Debian's own unit already runs /usr/bin/patroni on this path, as the
  # postgres user, and refuses to start without it. Nothing to override.
  systemctl enable --quiet patroni
}

# Patroni keeps postgresql.conf inside the data directory and renames
# whatever is there to postgresql.base.conf, so it can include it and
# write its own on top. Debian keeps the configuration in /etc instead,
# so an adopted Debian cluster has nothing for Patroni to rename and it
# stops with a FileNotFoundError before PostgreSQL ever starts. Put a
# copy where it expects one.
#
# hba_file and ident_file go: they point back into /etc, and Patroni
# manages pg_hba itself, in the data directory. include_dir goes with
# them, because the directory it names is over there too.
seed_pgdata_conf() { # seed_pgdata_conf <pgver> <pgdata>
  local src=/etc/postgresql/$1/main/postgresql.conf
  local dst=$2/postgresql.conf
  if [ -e "$dst" ] || [ -e "$2/postgresql.base.conf" ]; then
    ok "the data directory already has a postgresql.conf for Patroni to build on"
    return
  fi
  [ -r "$src" ] || die "no $src to copy; is this a Debian PostgreSQL?"
  sed -e "s|^\s*hba_file|#hba_file|" \
      -e "s|^\s*ident_file|#ident_file|" \
      -e "s|^\s*include_dir|#include_dir|" "$src" > "$dst"
  chown postgres:postgres "$dst"
  chmod 0600 "$dst"
  ok "copied Debian's postgresql.conf into the data directory for Patroni to take over"
}

wait_patroni() { # wait_patroni <what we are waiting to see>
  local i=0
  until patronictl -c "$PATRONI_CONF" list >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -ge 60 ] && die "Patroni did not answer; 'journalctl -u patroni' says why"
    sleep 2
  done
  ok "Patroni is answering"
}

# database.url names every server, so whichever takes writes is the one
# the controllers use and a promotion needs nothing changed here.
point_url_at_all() { # point_url_at_all <host1> <host2> ...
  local url creds dbpart port hosts=""
  url=$(cat "$SECRETS/database.url")
  creds=${url%@*}
  local rest=${url#*@}
  port=${rest%%/*}; port=${port##*:}; port=${port%%,*}
  dbpart=${rest#*/}; dbpart=${dbpart%%\?*}
  for h in "$@"; do hosts="$hosts,$h:$port"; done
  printf '%s@%s/%s?sslmode=disable&target_session_attrs=read-write' \
    "$creds" "${hosts#,}" "$dbpart" > "$SECRETS/database.url"
  chown root:forgesync "$SECRETS/database.url"; chmod 0640 "$SECRETS/database.url"
  ok "database.url names all $# servers"
  systemctl restart forgesyncd 2>/dev/null && ok "restarted forgesyncd" || true
}

# ------------------------------------------------------------------ init

init() {
  local second=${1:?usage: cluster.sh init <second machine> <third machine>}
  local third=${2:?usage: cluster.sh init <second machine> <third machine>}
  local me; me=$(this_address)
  [ -s "$SECRETS/database.url" ] || die "this machine does not look like a ForgeSync installation"

  local pgver pgdata pgport
  pgver=$(cluster_field 1); pgdata=$(cluster_field 6); pgport=$(cluster_field 3)
  [ -n "$pgver" ] || die "no PostgreSQL cluster here; run install.sh first"

  step "Taking a dump before anything is touched"
  local dump
  dump=/root/forgesync-before-cluster-$(date +%Y%m%d%H%M%S).dump
  if [ -x "$(dirname "$0")/backup.sh" ]; then
    "$(dirname "$0")/backup.sh" "$(dirname "$dump")" --verify >/dev/null \
      || die "the backup could not be verified; nothing has been changed"
    ok "backup.sh took and verified a dump"
  else
    PGPASSWORD=$(url_field password) pg_dump -h 127.0.0.1 -p "$pgport" -U forgesync \
      -d forgesync -Fc -f "$dump" || die "could not take a dump; nothing has been changed"
    ok "wrote $dump"
  fi
  note "if anything below goes wrong, that dump is the way back"

  # While the database is still running and reachable the ordinary way:
  # the role Patroni will stream with, and the password it will use.
  step "The replication role"
  local superpw replpw
  superpw=$(url_field password)
  if [ -s "$SECRETS/replication.pw" ]; then
    replpw=$(cat "$SECRETS/replication.pw")
    ok "reusing the replication password already here"
  else
    replpw=$(openssl rand -hex 24)
    umask 077; printf '%s' "$replpw" > "$SECRETS/replication.pw"
    chown root:forgesync "$SECRETS/replication.pw"; chmod 0640 "$SECRETS/replication.pw"
    ok "made a replication password"
  fi
  if as_postgres psql -p "$pgport" -qtAX -c "SELECT 1 FROM pg_roles WHERE rolname = '$REPL_USER'" 2>/dev/null | grep -q 1; then
    ok "replication role $REPL_USER exists"
  else
    printf "CREATE ROLE %s WITH REPLICATION LOGIN PASSWORD '%s'\n" "$REPL_USER" "$replpw" \
      | as_postgres psql -q -p "$pgport" -f - >/dev/null \
      || die "could not create the replication role; nothing has been changed"
    ok "created the replication role"
  fi

  packages

  step "etcd"
  # One member to start with, because the other two are not there yet.
  # Each join adds itself, so the cluster grows 1, 2, 3 and is only safe
  # to lose a machine once it is 3.
  write_etcd new "$(hostname -s)=http://$me:2380"
  warn "until all three have joined, losing a machine stops the database"

  step "Handing PostgreSQL to Patroni"
  systemctl stop "postgresql@$pgver-main" 2>/dev/null || true
  seed_pgdata_conf "$pgver" "$pgdata"
  write_patroni "$me:2379" "$superpw" "$replpw" "$pgver" "$pgdata" "$pgport"
  systemctl start patroni
  wait_patroni
  local i=0
  until [ "$(as_postgres psql -p "$pgport" -qtAX -c 'SELECT pg_is_in_recovery()' 2>/dev/null | tr -d ' ')" = f ]; do
    i=$((i + 1))
    [ "$i" -ge 60 ] && die "Patroni started but this machine is not the primary; 'patronictl list' says what it thinks"
    sleep 2
  done
  ok "Patroni took the existing database over and this machine is the primary"

  point_url_at_all "$me" "$second" "$third"

  umask 077
  {
    printf 'FORGESYNC-CLUSTER-1 %s %s %s %s %s\n' "$me" "$SCOPE" "$pgver" "$pgport" "$(hostname -s)"
    printf '%s\n%s\n' "$superpw" "$replpw"
  } > "$BUNDLE_DEFAULT"
  chmod 0600 "$BUNDLE_DEFAULT"

  cat <<DONE

  $(printf '\033[32mThe first machine is a one-member cluster.\033[0m') Add the other two.

    scp $BUNDLE_DEFAULT root@$second:/root/
    scp $BUNDLE_DEFAULT root@$third:/root/
    ssh root@$second 'cluster.sh join /root/$(basename "$BUNDLE_DEFAULT")'
    ssh root@$third  'cluster.sh join /root/$(basename "$BUNDLE_DEFAULT")'

  The bundle holds the database passwords. Delete it everywhere afterwards.
  Until all three have joined, this is less resilient than one machine was,
  not more: say so to anyone watching.

DONE
}

url_field() { # url_field password
  local url; url=$(cat "$SECRETS/database.url")
  local creds=${url#*://}; creds=${creds%%@*}
  printf '%s' "${creds#*:}"
}

# ------------------------------------------------------------------ join

join() {
  local bundle=${1:-$BUNDLE_DEFAULT}
  [ -s "$bundle" ] || die "$bundle is not there; run 'cluster.sh init' on the first machine"
  local mode; mode=$(stat -c '%a' "$bundle" 2>/dev/null || echo unknown)
  [ "$mode" = 600 ] || { chmod 0600 "$bundle"; warn "$bundle arrived mode $mode and has been shut to 0600"; }

  local header first scope pgver pgport superpw replpw me
  header=$(head -1 "$bundle")
  case "$header" in FORGESYNC-CLUSTER-1\ *) ;; *) die "$bundle is not a cluster bundle" ;; esac
  # shellcheck disable=SC2086 # five known fields
  set -- $header
  first=$2; scope=$3; pgver=$4; pgport=$5
  SCOPE=$scope
  superpw=$(sed -n '2p' "$bundle"); replpw=$(sed -n '3p' "$bundle")
  me=$(this_address)
  [ -n "$first" ] && [ -n "$superpw" ] && [ -n "$replpw" ] || die "$bundle is incomplete"

  step "Joining the cluster at $first"
  packages

  step "etcd"
  # As a learner, not a voter. A voting member counts towards the
  # majority the moment it is added, so adding one to a cluster of one
  # makes the majority two and nothing can be committed until the new
  # machine is up: a join that fails halfway then leaves the database
  # wedged, which is the opposite of what this is for. A learner
  # replicates but does not vote, and is promoted once it has caught up.
  local me_name; me_name=$(hostname -s)
  local added id
  if added=$(ETCDCTL_API=3 etcdctl --endpoints "http://$first:2379" member add "$me_name" \
       --peer-urls "http://$me:2380" --learner 2>&1); then
    id=$(etcd_member_id "$first" "$me")
    ok "added as a learner, so the cluster's majority has not changed"
  elif printf '%s' "$added" | grep -q "Peer URLs already exists"; then
    # Running this again after a join that stopped halfway.
    id=$(ETCDCTL_API=3 etcdctl --endpoints "http://$first:2379" member list \
      | awk -F', ' -v u="http://$me:2380" '$4 == u {print $1}')
    [ -n "$id" ] || die "etcd says this machine is already a member but will not say which one"
    if [ -d /var/lib/etcd/default/member ]; then
      ok "already a member of etcd, with its data here; carrying on from there"
    else
      # The cluster remembers this member at a raft index that the empty
      # data directory here cannot reach, and etcd panics with
      # "tocommit is out of range" rather than starting. Take the stale
      # member out and add it again, which is the only way back.
      note "this machine is registered in etcd but has no data of its own; replacing the member"
      ETCDCTL_API=3 etcdctl --endpoints "http://$first:2379" member remove "$id" >/dev/null \
        || die "could not remove the stale etcd member $id on $first"
      added=$(ETCDCTL_API=3 etcdctl --endpoints "http://$first:2379" member add "$me_name" \
        --peer-urls "http://$me:2380" --learner 2>&1) \
        || die "could not add this machine to etcd on $first ($added)"
      id=$(etcd_member_id "$first" "$me")
      ok "replaced the stale member and added this one as a learner"
    fi
  else
    die "could not add this machine to etcd on $first; is it reachable on 2379? ($added)"
  fi

  # The membership this machine starts with: the ones already named, plus
  # itself. A member that has just been added has no name in the listing
  # until it starts, so it has to be added here or etcd refuses to start
  # with "couldn't find local name in the initial cluster configuration".
  local members
  members=$(ETCDCTL_API=3 etcdctl --endpoints "http://$first:2379" member list \
    | awk -F', ' '$3 != "" {split($4, u, ","); printf "%s=%s,", $3, u[1]}')
  members=${members%,}
  [ -n "$members" ] || die "could not read the etcd membership from $first"
  # A member that has only just been added has no name in the listing yet,
  # so it is not in what came back and has to be added here; one that has
  # started once already is, and adding it again is a duplicate url that
  # etcd refuses to start with. Hence the test rather than always adding.
  case ",$members," in
    *",$me_name=http://$me:2380,"*) ;;
    *) members="$members,$me_name=http://$me:2380" ;;
  esac
  write_etcd existing "$members"

  # Promote once it has the data, which turns it into a voter and is what
  # actually makes the cluster more resilient than it was. A member that
  # is already a voter cannot be promoted, and asking would loop until it
  # gave up: this is the ordinary case when the join is run again.
  if [ -n "$id" ] && ! etcd_is_learner "$first" "$me"; then
    ok "already a voting member of etcd"
  elif [ -n "$id" ]; then
    local i=0
    until ETCDCTL_API=3 etcdctl --endpoints "http://$first:2379" member promote "$id" >/dev/null 2>&1; do
      i=$((i + 1))
      [ "$i" -ge 60 ] && die "this machine never caught up enough to be promoted in etcd; 'etcdctl member list' on $first shows where it got to"
      sleep 2
    done
    ok "promoted to a voting member"
  else
    warn "could not read the new member id, so it is still a learner; 'etcdctl member promote' on $first finishes it"
  fi

  step "PostgreSQL"
  local pgdata=/var/lib/postgresql/$pgver/main
  systemctl stop "postgresql@$pgver-main" 2>/dev/null || true
  if [ -d "$pgdata" ] && [ -n "$(ls -A "$pgdata" 2>/dev/null)" ]; then
    local keep
    keep="$pgdata.replaced-$(date +%Y%m%d%H%M%S)"
    mv "$pgdata" "$keep"
    ok "moved this machine's own cluster aside ($keep)"
  fi
  install -d -o postgres -g postgres -m 0700 "$pgdata"

  write_patroni "$first:2379,$me:2379" "$superpw" "$replpw" "$pgver" "$pgdata" "$pgport"
  systemctl start patroni
  wait_patroni
  local i=0
  until [ "$(as_postgres psql -p "$pgport" -qtAX -c 'SELECT pg_is_in_recovery()' 2>/dev/null | tr -d ' ')" = t ]; do
    i=$((i + 1))
    [ "$i" -ge 120 ] && die "this machine did not become a standby; 'patronictl list' and 'journalctl -u patroni' say why"
    sleep 2
  done
  ok "Patroni cloned the database from the leader and this machine is a standby"

  # The controllers should name every member, including this one.
  local hosts
  # One host per match, not one per line: patronictl prints the whole
  # list on a single line, and a line-based match would find only the
  # last of them.
  hosts=$(patronictl -c "$PATRONI_CONF" list -f json 2>/dev/null \
    | grep -o '"Host":[[:space:]]*"[^"]*"' \
    | sed 's/.*"\([^"]*\)"$/\1/' | sort -u | tr '\n' ' ')
  if [ -n "$hosts" ]; then
    # shellcheck disable=SC2086 # a list of hosts, split on purpose
    point_url_at_all $hosts
  else
    warn "could not list the members; leaving database.url alone"
  fi
  status
}

# ---------------------------------------------------------------- status

status() {
  step "The cluster"
  [ -s "$PATRONI_CONF" ] || die "Patroni is not set up here"
  patronictl -c "$PATRONI_CONF" list
  printf '\n'
  local n
  n=$(ETCDCTL_API=3 etcdctl member list 2>/dev/null | wc -l)
  case "$n" in
    3|5) ok "$n etcd members: a majority is $(( n / 2 + 1 )), so $(( n - (n / 2 + 1) )) can be lost" ;;
    1)   warn "1 etcd member: nothing is redundant yet, add the other two" ;;
    2|4) warn "$n etcd members: an even number, so losing one stops the database. Add one more" ;;
    *)   warn "could not count the etcd members" ;;
  esac
}

# ------------------------------------------------------------ switchover

switchover() {
  [ -s "$PATRONI_CONF" ] || die "Patroni is not set up here"
  step "Handing over"
  note "this is the planned kind: the primary stands down and another takes"
  note "over in a second or two. A machine that has died needs nothing done:"
  note "the other two notice and elect between themselves."
  patronictl -c "$PATRONI_CONF" switchover
  status
}

case "${1:-}" in
  init)       shift; init "$@" ;;
  join)       shift; join "$@" ;;
  status)     shift; status ;;
  switchover) shift; switchover ;;
  *) sed -n '2,28p' "$0"; exit 2 ;;
esac
