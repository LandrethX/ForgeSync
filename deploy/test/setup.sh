#!/usr/bin/env bash
# Starts the ForgeSync test environment and configures every Forgejo node:
#   - local admin accounts: siteadmin (human) and forgesync (service account)
#   - SceneID (Keycloak) as the only login source for regular users
#   - an API token for the forgesync account in .tokens/<node>.token
# Then smoke-tests connectivity. Safe to re-run.
#
# Usage: ./setup.sh           # nodes SE and DK
#        ./setup.sh --three   # also node DE
#
# Written for macOS bash 3.2 (no associative arrays).

set -euo pipefail
cd "$(dirname "$0")"
set -a; . ./.env; set +a

NODES="se dk"
PROFILE_ARGS=""
if [ "${1:-}" = "--three" ]; then
  NODES="se dk de"
  PROFILE_ARGS="--profile three"
fi

compose() { docker compose $PROFILE_ARGS "$@"; }
port_of() { case "$1" in se) echo 3001;; dk) echo 3002;; de) echo 3003;; esac; }
ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
FAILED=0

echo "==> Checking /etc/hosts entries"
missing=""
for h in sceneid.test $(for n in $NODES; do printf 'forgejo-%s.test ' "$n"; done); do
  if command -v dscacheutil >/dev/null; then
    dscacheutil -q host -a name "$h" | grep -q '127.0.0.1' || missing="$missing $h"
  else
    getent hosts "$h" | grep -q '127.0.0.1' || missing="$missing $h"
  fi
done
if [ -n "$missing" ]; then
  echo "Missing hostnames:$missing"
  echo "Add them with:"
  echo "  echo '127.0.0.1 sceneid.test forgejo-se.test forgejo-dk.test forgejo-de.test' | sudo tee -a /etc/hosts"
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

if [ ! -s .tokens/admin.token ]; then
  openssl rand -hex 32 > .tokens/admin.token
  chmod 600 .tokens/admin.token
  ok "generated ForgeSync admin API token -> .tokens/admin.token"
fi

echo "==> Smoke tests"
disco="http://sceneid.test:8080/realms/sceneid/.well-known/openid-configuration"
if curl -fsS "$disco" | grep -q '"issuer":"http://sceneid.test:8080/realms/sceneid"'; then
  ok "Mac -> SceneID discovery, issuer matches"
else
  fail "Mac -> SceneID discovery"
fi

for n in $NODES; do
  url="http://forgejo-$n.test:$(port_of "$n")"
  curl -fsS "$url/api/healthz" >/dev/null && ok "Mac -> $url healthy" || fail "Mac -> $url"

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

echo
if [ "$FAILED" -ne 0 ]; then
  echo "Some checks failed; see above. Logs: docker compose logs <service>"
  exit 1
fi
cat <<EOF
Ready.
  SceneID admin console : http://sceneid.test:8080/admin   ($SCENEID_ADMIN_USER / $SCENEID_ADMIN_PASSWORD)
$(for n in $NODES; do echo "  Forgejo $(echo "$n" | tr a-z A-Z)            : http://forgejo-$n.test:$(port_of "$n")   (ssh port 222$(port_of "$n" | cut -c4))"; done)
  SceneID test users    : alice / alice-pw, bob / bob-pw, carol / carol-pw
  Local admins per node : siteadmin / $FORGEJO_SITEADMIN_PASSWORD, forgesync (API tokens in .tokens/)
  ForgeSync database    : localhost:5432 (forgesync / $FORGESYNC_DB_PASSWORD)
  Run the controller    : go run ./cmd/forgesyncd -config deploy/test/forgesync.yaml   (from the repo root)
EOF
