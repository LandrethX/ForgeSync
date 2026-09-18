#!/usr/bin/env bash
# Starts the ForgeSync test environment and configures every Forgejo node:
#   - local admin accounts: siteadmin (human) and forgesync (service account)
#   - SceneID (Keycloak) as the only login source for regular users
#   - an API token for the forgesync account in .tokens/<node>.token
# Then smoke-tests connectivity. Safe to re-run.
#
# Then builds and starts the ForgeSync controller (with its admin UI) as a
# container.
#
# Usage: ./setup.sh           # nodes SE and DK
#        ./setup.sh --three   # also node DE
#        ./setup.sh --all     # SE, DK, DE, UK and US
#
# To reach the environment from other machines (e.g. on a server), run it as
# PUBLIC_BIND=0.0.0.0 ./setup.sh and add the hostnames on those machines too.
#
# Written for macOS bash 3.2 (no associative arrays).

set -euo pipefail
cd "$(dirname "$0")"
set -a; . ./.env; set +a

NODES="se dk"
PROFILE_ARGS=""
case "${1:-}" in
  --three) NODES="se dk de"; PROFILE_ARGS="--profile three" ;;
  --all) NODES="se dk de uk us"; PROFILE_ARGS="--profile all" ;;
  "") ;;
  *) echo "usage: $0 [--three|--all]" >&2; exit 2 ;;
esac

compose() { docker compose $PROFILE_ARGS "$@"; }
port_of() { case "$1" in se) echo 3001;; dk) echo 3002;; de) echo 3003;; uk) echo 3004;; us) echo 3005;; esac; }
ssh_of()  { case "$1" in se) echo 2221;; dk) echo 2222;; de) echo 2223;; uk) echo 2224;; us) echo 2225;; esac; }
site_of() { echo "$1" | tr a-z A-Z; }
ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
FAILED=0

echo "==> Checking /etc/hosts entries"
missing=""
for h in sceneid.test forgesync.test $(for n in $NODES; do printf 'forgejo-%s.test ' "$n"; done); do
  if command -v dscacheutil >/dev/null; then
    dscacheutil -q host -a name "$h" | grep -q '127.0.0.1' || missing="$missing $h"
  else
    getent hosts "$h" | grep -q '127.0.0.1' || missing="$missing $h"
  fi
done
if [ -n "$missing" ]; then
  echo "Missing hostnames:$missing"
  echo "Add them with:"
  echo "  echo '127.0.0.1 sceneid.test forgesync.test forgejo-se.test forgejo-dk.test forgejo-de.test forgejo-uk.test forgejo-us.test' | sudo tee -a /etc/hosts"
  exit 1
fi
ok "hostnames resolve to 127.0.0.1"

echo "==> Starting containers (first run pulls images)"
compose up -d --wait

mkdir -p .tokens
chmod 700 .tokens

for n in $NODES; do
  svc="forgejo-$n"
  echo "==> Configuring $svc"
  fj() { compose exec -T -u git "$svc" forgejo "$@"; }

  admins=$(fj admin user list --admin | awk 'NR>1 {print $2}')
  for u in siteadmin forgesync; do
    if echo "$admins" | grep -qx "$u"; then
      ok "local admin '$u' exists"
    else
      if [ "$u" = siteadmin ]; then pw=$FORGEJO_SITEADMIN_PASSWORD; else pw=$FORGEJO_FORGESYNC_PASSWORD; fi
      fj admin user create --admin --username "$u" --password "$pw" \
        --email "$u@forgejo-$n.test" --must-change-password=false >/dev/null
      ok "created local admin '$u'"
    fi
  done

  if fj admin auth list | grep -qw SceneID; then
    ok "SceneID login source exists"
  else
    fj admin auth add-oauth --name SceneID --provider openidConnect \
      --key forgejo --secret "$SCENEID_FORGEJO_CLIENT_SECRET" \
      --auto-discover-url "http://sceneid.test:8080/realms/sceneid/.well-known/openid-configuration" \
      --scopes profile --scopes email >/dev/null
    # The running server loads login sources at startup; restart so it
    # picks up the one added from the CLI.
    compose restart "$svc" >/dev/null
    compose up -d --wait "$svc" >/dev/null
    ok "added SceneID login source (node restarted)"
  fi

  tokfile=".tokens/$n.token"
  if [ -s "$tokfile" ]; then
    ok "API token already in $tokfile"
  else
    fj admin user generate-access-token --username forgesync \
      --token-name "forgesync-$(date +%Y%m%d%H%M%S)" --scopes all --raw > "$tokfile"
    chmod 600 "$tokfile"
    ok "generated API token -> $tokfile"
  fi
done

printf '%s' "$SCENEID_FORGESYNC_ADMIN_CLIENT_SECRET" > .tokens/sceneid-admin-client.secret
chmod 600 .tokens/sceneid-admin-client.secret

if [ ! -s .tokens/admin.token ]; then
  openssl rand -hex 32 > .tokens/admin.token
  chmod 600 .tokens/admin.token
  ok "generated ForgeSync admin API token -> .tokens/admin.token"
fi
if [ ! -s .tokens/webhook.secret ]; then
  openssl rand -hex 32 > .tokens/webhook.secret
  chmod 600 .tokens/webhook.secret
  ok "generated the webhook secret -> .tokens/webhook.secret"
fi

echo "==> Building and starting the ForgeSync controller (first build takes a few minutes)"
FORGESYNC_VERSION=$(git -C ../.. describe --tags --always --dirty 2>/dev/null || echo dev)
FORGESYNC_COMMIT=$(git -C ../.. rev-parse --short HEAD 2>/dev/null || echo unknown)
export FORGESYNC_VERSION FORGESYNC_COMMIT
# The controller watches exactly the nodes started here.
mkdir -p .work
{
  cat forgesync.docker.yaml
  echo "nodes:"
  for n in $NODES; do
    # The SceneID login source's id differs per node; ForgeSync needs it to
    # create SceneID users there.
    src=$(compose exec -T -u git "forgejo-$n" forgejo admin auth list | awk '$2 == "SceneID" {print $1}')
    printf '  - name: %s\n    site: %s\n    url: http://forgejo-%s.test:%s\n    token_file: /etc/forgesync/tokens/%s.token\n    sceneid_source_id: %s\n' \
      "$n" "$(site_of "$n")" "$n" "$(port_of "$n")" "$n" "${src:-0}"
  done
} > .work/forgesync.docker.yaml
docker compose $PROFILE_ARGS --profile controller up -d --build --force-recreate --wait forgesync
ok "controller running ($FORGESYNC_VERSION)"

echo "==> Smoke tests"
disco="http://sceneid.test:8080/realms/sceneid/.well-known/openid-configuration"
if curl -fsS "$disco" | grep -q '"issuer":"http://sceneid.test:8080/realms/sceneid"'; then
  ok "host -> SceneID discovery, issuer matches"
else
  fail "host -> SceneID discovery"
fi

for n in $NODES; do
  url="http://forgejo-$n.test:$(port_of "$n")"
  curl -fsS "$url/api/healthz" >/dev/null && ok "host -> $url healthy" || fail "host -> $url"

  tok=$(cat ".tokens/$n.token")
  login=$(curl -fsS -H "Authorization: token $tok" "$url/api/v1/user" | sed -n 's/.*"login":"\([^"]*\)".*/\1/p')
  [ "$login" = forgesync ] && ok "API token on $n authenticates as forgesync" || fail "API token on $n (got '$login')"
  curl -fsS -o /dev/null -H "Authorization: token $tok" "$url/api/v1/admin/users" \
    && ok "forgesync has admin API access on $n" || fail "admin API on $n"

  compose exec -T "forgejo-$n" curl -fsS -o /dev/null "$disco" \
    && ok "forgejo-$n -> SceneID reachable" || fail "forgejo-$n -> SceneID"
  for m in $NODES; do
    [ "$m" = "$n" ] && continue
    compose exec -T "forgejo-$n" curl -fsS -o /dev/null "http://forgejo-$m.test:$(port_of "$m")/api/healthz" \
      && ok "forgejo-$n -> forgejo-$m reachable" || fail "forgejo-$n -> forgejo-$m"
  done
done

if curl -fsS http://forgesync.test:8090/readyz | grep -q ready; then
  ok "host -> ForgeSync controller ready"
else
  fail "ForgeSync controller not ready (docker compose --profile controller logs forgesync)"
fi
compose exec -T forgejo-se curl -fsS -o /dev/null http://forgesync.test:8090/healthz \
  && ok "forgejo-se -> ForgeSync controller reachable" || fail "forgejo-se -> ForgeSync controller"

echo
if [ "$FAILED" -ne 0 ]; then
  echo "Some checks failed; see above. Logs: docker compose logs <service>"
  exit 1
fi
cat <<EOF
Ready.
  SceneID admin console : http://sceneid.test:8080/admin   ($SCENEID_ADMIN_USER / $SCENEID_ADMIN_PASSWORD)
$(for n in $NODES; do echo "  Forgejo $(site_of "$n")            : http://forgejo-$n.test:$(port_of "$n")   (ssh port $(ssh_of "$n"))"; done)
  SceneID test users    : alice / alice-pw (ForgeSync administrator), bob / bob-pw (operator),
                          carol / carol-pw (viewer), erin / erin-pw (no ForgeSync role)
  Local admins per node : siteadmin / $FORGEJO_SITEADMIN_PASSWORD, forgesync (API tokens in .tokens/)
  ForgeSync admin UI    : http://forgesync.test:8090   (sign in with SceneID, or "Use the admin token instead")
  Admin token           : .tokens/admin.token
  ForgeSync database    : localhost:5432 (forgesync / $FORGESYNC_DB_PASSWORD)
  Controller logs       : docker compose --profile controller logs -f forgesync
EOF
