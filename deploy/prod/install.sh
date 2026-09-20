#!/usr/bin/env bash
# Installs ForgeSync on a fresh Debian 13 machine, which in practice means
# an unprivileged Proxmox LXC. It does the whole of deploy/prod/README.md
# sections 1 to 7: packages, a Go and Node toolchain, the source, the
# build, PostgreSQL, the user and directories, the secrets, the config,
# the systemd unit, and a check that the thing actually answers.
#
#   curl -fsSL https://raw.githubusercontent.com/LandrethX/ForgeSync/main/deploy/prod/install.sh | bash
#
# Reading a script before running it as root is a reasonable habit, and
# this one is written to be read:
#
#   curl -fsSLO https://raw.githubusercontent.com/LandrethX/ForgeSync/main/deploy/prod/install.sh
#   less install.sh && bash install.sh
#
# It asks whether this is the first ForgeSync machine. If it is not, it
# asks where the first one is and joins this machine to it: same database,
# same secrets, its own name and priority. Answer with --first or
# --join <address> to skip the questions, which is what you want when this
# runs from a pipe.
#
# Options, all optional:
#   --first                               this is the first machine.
#   --join 10.0.0.1                       this is not: an address or URL
#                                         for the first one. Implies that
#                                         PostgreSQL lives there.
#   --url https://forgesync.example.org   how the Forgejo nodes reach this
#                                         controller for webhooks, and how
#                                         people reach the UI. Default: the
#                                         machine's own address on :8090.
#   --name forgesync-a                    this controller's name. Default:
#                                         the host name.
#   --priority 2                          which controller should lead;
#                                         lower wins. Default: 1 on the
#                                         first machine, and on a joining
#                                         one the next number free.
#   --ref main                            the branch or tag to build.
#   --no-postgres                         the database is somewhere else;
#                                         put its URL in
#                                         /etc/forgesync/secrets/database.url
#                                         before running this.
#   --keep-build                          leave the build caches in place
#                                         (about 320 MB) for the next build.
#
# It is safe to run again: nothing already there is overwritten. Secrets
# are generated only when missing, the config is written only when
# missing, and the database role and database are created only when they
# do not exist. Re-running is how you upgrade.
#
# Joining a second machine gives you a controller that survives losing the
# first. It does not make the database redundant: that is still one server,
# on the first machine, and it takes three machines to change (section 12
# of deploy/prod/README.md). Setting up the replicated database is not
# automated yet.
set -Eeuo pipefail

FORGESYNC_REPO=${FORGESYNC_REPO:-https://github.com/LandrethX/ForgeSync.git}
FORGESYNC_REF=${FORGESYNC_REF:-main}
# Node is not read from the repository the way Go is, because nothing in
# the repository pins it. Vite 8 wants 20.19 or newer; this is the current
# release line and what the UI is built with.
NODE_VERSION=${NODE_VERSION:-22.22.2}

ETC=/etc/forgesync
SECRETS=$ETC/secrets
STATE=/var/lib/forgesync
SRC=/usr/local/src/forgesync
GOROOT=/usr/local/go
NODEROOT=/opt/node
PUBLIC_URL=""
CONTROLLER_NAME=""
WITH_POSTGRES=1
KEEP_BUILD=0
ROLE=""            # first | join, asked for when not given
PRIMARY=""         # where the first machine is, when joining
PRIORITY=""         # lower leads; the first machine is 1

while [ $# -gt 0 ]; do
  case "$1" in
    --first)       ROLE="first"; shift ;;
    --join)        ROLE="join"; PRIMARY=${2:?--join needs the address of the first machine}; shift 2 ;;
    --priority)    PRIORITY=${2:?--priority needs a number}; shift 2 ;;
    --url)         PUBLIC_URL=${2:?--url needs a value}; shift 2 ;;
    --name)        CONTROLLER_NAME=${2:?--name needs a value}; shift 2 ;;
    --ref)         FORGESYNC_REF=${2:?--ref needs a value}; shift 2 ;;
    --no-postgres) WITH_POSTGRES=0; shift ;;
    --keep-build)  KEEP_BUILD=1; shift ;;
    -h|--help)     sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown option: $1 (try --help)" >&2; exit 2 ;;
  esac
done

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
note() { printf '        %s\n' "$*"; }
warn() { printf '  \033[33mnote\033[0m  %s\n' "$*"; }
die()  { printf '  \033[31mstop\033[0m  %s\n' "$*" >&2; exit 1; }

# ------------------------------------------------------------- preflight

step "Checking the machine"
[ "$(id -u)" = 0 ] || die "run this as root"

. /etc/os-release 2>/dev/null || die "this does not look like a Linux with /etc/os-release"
case "${ID:-}:${VERSION_ID:-}" in
  debian:13) ok "Debian 13 ($VERSION_CODENAME)" ;;
  *) die "this installs on Debian 13; found ${PRETTY_NAME:-something else}" ;;
esac

case "$(uname -m)" in
  x86_64)  ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m); Go and Node are fetched for amd64 or arm64" ;;
esac
ok "architecture $ARCH"

# What this needs before it can install anything. All of these are in a
# Debian base system; checking says which one is missing rather than
# failing halfway through with something obscure.
missing=""
for c in apt-get awk sed tar df id hostname sha256sum mktemp; do
  command -v "$c" >/dev/null 2>&1 || missing="$missing $c"
done
[ -z "$missing" ] || die "these are needed and not here:$missing"
ok "the base tools are here"

# Running a command as the postgres user. runuser is util-linux, so it is
# always present on Debian; sudo usually is too, and a production machine
# will have it. Either will do, and saying which is used beats assuming.
if command -v runuser >/dev/null 2>&1; then
  as_postgres() { runuser -u postgres -- "$@"; }
  PG_AS="runuser"
elif command -v sudo >/dev/null 2>&1; then
  as_postgres() { sudo -u postgres -- "$@"; }
  PG_AS="sudo"
else
  as_postgres() { die "neither runuser nor sudo is here, so PostgreSQL cannot be set up"; }
  PG_AS="neither"
fi
if [ "$WITH_POSTGRES" -eq 1 ] || [ -z "$ROLE" ]; then
  case "$PG_AS" in
    neither) warn "neither runuser nor sudo found; install one of them if this machine runs PostgreSQL" ;;
    *)       ok "will run PostgreSQL commands with $PG_AS" ;;
  esac
fi

free_mb=$(df -Pm / | awk 'NR==2 {print $4}')
[ "$free_mb" -ge 3000 ] || die "only ${free_mb} MB free on /; the build needs about 3 GB, and a 16 GB disk is the usual size"
ok "${free_mb} MB free on /"

mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
if [ "$mem_mb" -lt 1800 ]; then
  warn "${mem_mb} MB of memory; the build wants about 1 GB free and the usual size is 2 GB"
else
  ok "${mem_mb} MB of memory"
fi

# --------------------------------------------------------- first or not

step "Which machine is this"
if [ -z "$ROLE" ]; then
  if [ -t 0 ]; then
    printf '  Is this the first ForgeSync machine? [Y/n] '
    read -r answer
    case "${answer:-y}" in
      [Nn]*) ROLE="join" ;;
      *)     ROLE="first" ;;
    esac
  else
    ROLE="first"
    note "not a terminal, so assuming the first machine; --join <address> says otherwise"
  fi
fi

if [ "$ROLE" = join ]; then
  while [ -z "$PRIMARY" ]; do
    printf '  Address or URL of the first ForgeSync machine: '
    read -r PRIMARY
  done
  # An address, a host name or a whole URL are all reasonable answers.
  case "$PRIMARY" in
    http://*|https://*) PRIMARY_URL=$PRIMARY ;;
    *:*)                PRIMARY_URL="http://$PRIMARY" ;;
    *)                  PRIMARY_URL="http://$PRIMARY:8090" ;;
  esac
  PRIMARY_HOST=${PRIMARY_URL#*://}; PRIMARY_HOST=${PRIMARY_HOST%%:*}; PRIMARY_HOST=${PRIMARY_HOST%%/*}
  WITH_POSTGRES=0
  ok "joining the installation at $PRIMARY_URL"
  if curl -fsS -m 5 "$PRIMARY_URL/healthz" >/dev/null 2>&1; then
    ok "it answers"
  else
    warn "$PRIMARY_URL/healthz did not answer; carrying on, but check it before this one starts"
  fi
else
  ok "the first machine: PostgreSQL and the secrets are made here"
fi

# ------------------------------------------------------------- packages

step "Packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
pkgs="ca-certificates curl git openssl xz-utils"
[ "$WITH_POSTGRES" -eq 1 ] && pkgs="$pkgs postgresql"
# shellcheck disable=SC2086 # a deliberate word-split of the package list
apt-get install -y -qq $pkgs >/dev/null
ok "installed: $pkgs"
missing=""
for c in git curl openssl tar; do
  command -v "$c" >/dev/null 2>&1 || missing="$missing $c"
done
if [ "$WITH_POSTGRES" -eq 1 ]; then
  for c in pg_lsclusters psql; do
    command -v "$c" >/dev/null 2>&1 || missing="$missing $c"
  done
fi
[ -z "$missing" ] || die "apt said it installed everything, but these are still missing:$missing"
ok "git $(git --version | awk '{print $3}') (replication runs the git CLI, so it is a runtime dependency)"

# ------------------------------------------------------------- source

step "Source"
if [ -d "$SRC/.git" ]; then
  git -C "$SRC" remote set-url origin "$FORGESYNC_REPO"
  git -C "$SRC" fetch --quiet --tags origin
  git -C "$SRC" checkout --quiet --force "$FORGESYNC_REF"
  git -C "$SRC" reset --quiet --hard "origin/$FORGESYNC_REF" 2>/dev/null || true
  ok "updated $SRC to $FORGESYNC_REF"
else
  mkdir -p "$(dirname "$SRC")"
  git clone --quiet "$FORGESYNC_REPO" "$SRC"
  git -C "$SRC" checkout --quiet "$FORGESYNC_REF"
  ok "cloned $FORGESYNC_REPO at $FORGESYNC_REF"
fi
VERSION=$(git -C "$SRC" describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT=$(git -C "$SRC" rev-parse --short HEAD)
ok "building $VERSION ($COMMIT)"

# ------------------------------------------------------------- toolchains

# The Go version comes from the repository, so this script does not go
# stale when the project moves to a newer one. Debian 13 ships 1.24, which
# is older than go.mod asks for, so it has to come from go.dev.
GO_VERSION=$(awk '/^go [0-9]/ {print $2; exit}' "$SRC/go.mod")
[ -n "$GO_VERSION" ] || die "could not read the Go version from $SRC/go.mod"

fetch_verified() { # fetch_verified <url> <sha256url-or-sha> <dest>
  local url=$1 want=$2 dest=$3 sum
  curl -fsSL --retry 3 -o "$dest" "$url"
  case "$want" in
    http*) sum=$(curl -fsSL --retry 3 "$want" | awk '{print $1}' | head -1) ;;
    *)     sum=$want ;;
  esac
  [ -n "$sum" ] || die "no checksum for $url"
  echo "$sum  $dest" | sha256sum -c - >/dev/null 2>&1 \
    || die "$url did not match its published checksum"
}

step "Go $GO_VERSION"
if [ -x "$GOROOT/bin/go" ] && "$GOROOT/bin/go" version | grep -q "go$GO_VERSION "; then
  ok "already installed"
else
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  tarball="go${GO_VERSION}.linux-${ARCH}.tar.gz"
  fetch_verified "https://dl.google.com/go/$tarball" \
                 "https://dl.google.com/go/$tarball.sha256" "$tmp/go.tar.gz"
  rm -rf "$GOROOT"
  tar -C /usr/local -xzf "$tmp/go.tar.gz"
  rm -rf "$tmp"; trap - EXIT
  ok "installed $("$GOROOT/bin/go" version)"
fi

step "Node $NODE_VERSION"
if [ -x "$NODEROOT/bin/node" ] && [ "$("$NODEROOT/bin/node" --version)" = "v$NODE_VERSION" ]; then
  ok "already installed"
else
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  base="node-v${NODE_VERSION}-linux-${ARCH/amd64/x64}"
  want=$(curl -fsSL --retry 3 "https://nodejs.org/dist/v$NODE_VERSION/SHASUMS256.txt" \
         | awk -v f="$base.tar.xz" '$2 == f {print $1}')
  [ -n "$want" ] || die "no published checksum for $base.tar.xz"
  fetch_verified "https://nodejs.org/dist/v$NODE_VERSION/$base.tar.xz" "$want" "$tmp/node.tar.xz"
  rm -rf "$NODEROOT"
  mkdir -p "$NODEROOT"
  tar -C "$NODEROOT" --strip-components=1 -xJf "$tmp/node.tar.xz"
  rm -rf "$tmp"; trap - EXIT
  ok "installed $("$NODEROOT/bin/node" --version)"
fi
export PATH="$GOROOT/bin:$NODEROOT/bin:$PATH"

# ------------------------------------------------------------- build

step "Building"
note "the admin UI is embedded in the controller binary, so the UI is built first"
( cd "$SRC/web" && npm ci --no-audit --no-fund --silent && npm run build --silent ) >/dev/null
ok "admin UI"
( cd "$SRC" && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X scenegit.org/forgesync/internal/buildinfo.Version=$VERSION -X scenegit.org/forgesync/internal/buildinfo.Commit=$COMMIT" \
    -o /tmp/forgesync-build/ ./cmd/... )
install -m 0755 /tmp/forgesync-build/forgesyncd /tmp/forgesync-build/forgesync /usr/local/bin/
rm -rf /tmp/forgesync-build
ok "$(/usr/local/bin/forgesyncd -version 2>&1 | head -1)"

# ------------------------------------------------------- user and layout

step "User and directories"
if id forgesync >/dev/null 2>&1; then
  ok "user forgesync exists"
else
  useradd --system --home "$STATE" --shell /usr/sbin/nologin forgesync
  ok "created the forgesync system user"
fi
install -d -o forgesync -g forgesync -m 0750 "$STATE" "$ETC"
install -d -o root -g forgesync -m 0750 "$SECRETS"
ok "$STATE, $ETC and $SECRETS"

# ------------------------------------------------------------- database

PGPORT=""
if [ "$WITH_POSTGRES" -eq 1 ]; then
  step "PostgreSQL"
  # Installing the package makes a cluster but does not always leave it
  # running, and a machine that has been rebooted mid-install will not have
  # started it either. Start it rather than assume.
  pgport_now() { pg_lsclusters -h 2>/dev/null | awk '$4 == "online" {print $3; exit}'; }
  PGPORT=$(pgport_now)
  if [ -z "$PGPORT" ]; then
    systemctl enable --quiet --now postgresql 2>/dev/null || systemctl start postgresql 2>/dev/null || true
    i=0
    while [ -z "$PGPORT" ] && [ "$i" -lt 30 ]; do
      sleep 1; i=$((i + 1)); PGPORT=$(pgport_now)
    done
    [ -n "$PGPORT" ] && ok "started the cluster"
  fi
  [ -n "$PGPORT" ] || die "no online PostgreSQL cluster. 'pg_lsclusters' says what state it is in, and 'journalctl -u postgresql' why"
  systemctl enable --quiet postgresql 2>/dev/null || true
  ok "cluster on port $PGPORT, and set to start at boot"

  if as_postgres psql -qtAX -c "SELECT 1 FROM pg_roles WHERE rolname = 'forgesync'" | grep -q 1; then
    ok "role forgesync exists"
  else
    PW=$(openssl rand -hex 24)   # hex, so it needs no percent-encoding in a URL
    # Through stdin, not an argument: anything on a command line is in
    # /proc and visible to every user on the machine while it runs.
    printf "CREATE ROLE forgesync LOGIN PASSWORD '%s'\n" "$PW" | as_postgres psql -q -f - >/dev/null
    printf 'postgres://forgesync:%s@127.0.0.1:%s/forgesync?sslmode=disable' "$PW" "$PGPORT" \
      > "$SECRETS/database.url"
    unset PW
    ok "created the role and wrote $SECRETS/database.url"
  fi
  if as_postgres psql -qtAX -c "SELECT 1 FROM pg_database WHERE datname = 'forgesync'" | grep -q 1; then
    ok "database forgesync exists"
  else
    as_postgres psql -qc "CREATE DATABASE forgesync OWNER forgesync" >/dev/null
    ok "created the database"
  fi
  # backup.sh --verify restores a dump into a scratch database to check it.
  as_postgres psql -qc "ALTER ROLE forgesync CREATEDB" >/dev/null
  ok "the role may create a database, so backup.sh --verify can check its own dumps"
fi
if [ "$ROLE" = join ]; then
  step "Secrets from the first machine"
  note "controllers share one database, and three of these have to be identical"
  note "or the CLI works on one machine only, the webhooks are rewritten on"
  note "every failover, and the node tokens in the database will not open."
  printf '\n  On %s, run:\n\n' "$PRIMARY_HOST"
  printf '    scp /etc/forgesync/secrets/{database.url,admin.token,webhook.secret,node-key} \\\n'
  printf '        root@%s:%s/\n\n' "$(hostname -I 2>/dev/null | awk '{print $1}')" "$SECRETS"
  i=0
  until [ -s "$SECRETS/database.url" ] && [ -s "$SECRETS/admin.token" ] \
     && [ -s "$SECRETS/webhook.secret" ] && [ -s "$SECRETS/node-key" ]; do
    i=$((i + 1))
    [ "$i" -eq 1 ] && printf '  waiting for the four files'
    printf '.'
    [ "$i" -ge 600 ] && { printf '\n'; die "the secrets did not arrive"; }
    sleep 1
  done
  [ "$i" -gt 0 ] && printf '\n'
  ok "all four are here"

  # The first machine's own URL says 127.0.0.1, which means something else
  # from here. Point it at the machine it actually lives on.
  url=$(cat "$SECRETS/database.url")
  case "$url" in
    *@127.0.0.1:*|*@localhost:*)
      printf '%s' "${url/@127.0.0.1:/@$PRIMARY_HOST:}" > "$SECRETS/database.url"
      printf '%s' "$(sed "s|@localhost:|@$PRIMARY_HOST:|" "$SECRETS/database.url")" > "$SECRETS/database.url"
      ok "pointed database.url at $PRIMARY_HOST instead of the loopback"
      note "PostgreSQL there has to accept it: listen_addresses in postgresql.conf"
      note "and a host line for this machine in pg_hba.conf, then reload it"
      ;;
    *) ok "database.url already names a host this machine can reach" ;;
  esac
  unset url
fi

[ -s "$SECRETS/database.url" ] || die "no $SECRETS/database.url; with --no-postgres you have to write it yourself"

# ------------------------------------------------------------- secrets

step "Secrets"
make_secret() { # make_secret <file> <description>
  if [ -s "$1" ]; then
    ok "$(basename "$1") is already there, left alone"
  else
    openssl rand -hex 32 > "$1"
    ok "generated $(basename "$1"): $2"
  fi
}
make_secret "$SECRETS/admin.token" "the CLI's bearer token, and the break-glass sign-in"
make_secret "$SECRETS/webhook.secret" "signs the webhooks the nodes send back"
make_secret "$SECRETS/node-key" "seals node tokens in the database"
chown root:forgesync "$SECRETS"/*
chmod 0640 "$SECRETS"/*
ok "all owned root:forgesync, mode 0640"
note "back up node-key somewhere other than the database dump, or the two are lost together"

# -------------------------------------------------------------- config

step "Config"
[ -n "$CONTROLLER_NAME" ] || CONTROLLER_NAME=$(hostname -s)

# Lower priority leads. The first machine is 1; a joining one takes the
# next number free, which the first machine can be asked for, since the
# admin token has just been copied across.
if [ -z "$PRIORITY" ]; then
  PRIORITY=1
  if [ "$ROLE" = join ]; then
    taken=$(curl -fsS -m 5 -H "Authorization: Bearer $(cat "$SECRETS/admin.token")" \
            "$PRIMARY_URL/api/v1/overview" 2>/dev/null \
            | tr ',' '\n' | sed -n 's/.*"priority":\([0-9]*\).*/\1/p' | sort -n | tail -1)
    if [ -n "$taken" ]; then
      PRIORITY=$((taken + 1))
      ok "the installation already has controllers up to priority $taken, so this one is $PRIORITY"
    else
      PRIORITY=2
      warn "could not ask $PRIMARY_URL which priorities are taken; using $PRIORITY"
    fi
  fi
fi
ok "controller $CONTROLLER_NAME, priority $PRIORITY"
if [ "$PRIORITY" -eq 3 ]; then
  note "three machines is where the database can be made redundant too;"
  note "that part is section 12 of deploy/prod/README.md and is still by hand"
fi
if [ -z "$PUBLIC_URL" ]; then
  ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}')
  PUBLIC_URL="http://${ip:-127.0.0.1}:8090"
  note "no --url given, so using $PUBLIC_URL"
  note "the Forgejo nodes have to reach that address for webhooks to work"
fi

if [ -s "$ETC/forgesync.yaml" ]; then
  ok "$ETC/forgesync.yaml is already there, left alone"
else
  cat > "$ETC/forgesync.yaml" <<YAML
# ForgeSync, installed by deploy/prod/install.sh on $(date -u +%Y-%m-%d).
# deploy/prod/forgesync.yaml in the source is the annotated version: it
# explains every setting, including the ones left at their defaults here.
controller:
  name: $CONTROLLER_NAME
  url: $PUBLIC_URL
  # Lower leads. A second machine gets 2, a third 3.
  priority: $PRIORITY

log:
  level: info
  format: text

http:
  listen: 0.0.0.0:8090
  admin_token_file: $SECRETS/admin.token
  # Set this once there is TLS in front of or on this controller. Without
  # it the session cookie is sent over plain HTTP.
  secure_cookies: false

# The key that seals node tokens in the database. Every controller needs
# the same file; see section 4 of deploy/prod/README.md.
node_key_file: $SECRETS/node-key

database:
  url_file: $SECRETS/database.url

webhooks:
  url: $PUBLIC_URL/api/v1/hooks/forgejo
  secret_file: $SECRETS/webhook.secret

replication:
  enabled: true
  work_dir: $STATE/git

# Nodes live in ForgeSync's database and are added from the admin UI, so
# there is no list here. One can still be written out in a nodes: block if
# you would rather, and it is taken into the database on the next start.
YAML
  chown root:forgesync "$ETC/forgesync.yaml"
  chmod 0640 "$ETC/forgesync.yaml"
  ok "wrote $ETC/forgesync.yaml"
fi

# ------------------------------------------------------------- service

step "Service"
install -m 0644 "$SRC/deploy/prod/forgesyncd.service" /etc/systemd/system/forgesyncd.service
systemctl daemon-reload
systemctl enable --quiet forgesyncd
systemctl restart forgesyncd
ok "forgesyncd enabled and started"

# -------------------------------------------------------------- check

step "Checking it answers"
i=0
until curl -fsS -m 3 "http://127.0.0.1:8090/readyz" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 30 ]; then
    printf '\n'
    journalctl -u forgesyncd -n 30 --no-pager || true
    die "it did not become ready; the log is above"
  fi
  sleep 1
done
ok "/readyz answers, so the database is reachable and takes writes"
ok "$(curl -fsS -m 3 http://127.0.0.1:8090/healthz)"

# ------------------------------------------------------------- tidy up

if [ "$KEEP_BUILD" -eq 0 ]; then
  step "Reclaiming the build space"
  before=$(df -Pm / | awk 'NR==2 {print $4}')
  go clean -cache -modcache >/dev/null 2>&1 || true
  rm -rf "$SRC/web/node_modules" /root/.npm
  after=$(df -Pm / | awk 'NR==2 {print $4}')
  ok "gave back $((after - before)) MB; --keep-build skips this when you build often"
fi

# ------------------------------------------------------------ next steps

cat <<DONE

$(printf '\033[32mForgeSync is running.\033[0m')

  UI and API   $PUBLIC_URL
  Config       $ETC/forgesync.yaml
  Secrets      $SECRETS
  Logs         journalctl -u forgesyncd -f

Two things to do next.

1. Make an account to sign in with. There is nobody to make the first one,
   so it is made with the admin token:

   curl -fsS -X POST $PUBLIC_URL/api/v1/accounts \\
     -H "Authorization: Bearer \$(cat $SECRETS/admin.token)" \\
     -H 'Content-Type: application/json' \\
     -d '{"username":"you","password":"a long one","role":"administrator"}'

   The token is read from the file rather than printed here, so it does not
   end up in a terminal scrollback or a log of this install.

2. Add your Forgejo nodes. Each needs a site-admin account called
   forgesync on that node and an API token for it. Section 9 of
   deploy/prod/README.md has what to set on the node itself, including
   putting this machine in [webhook] ALLOWED_HOST_LIST.

Put TLS in front of this before anyone signs in over a network: section 8.
DONE
