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
#        ./setup.sh --all --standby   # ... and a second controller on :8091
#        ./setup.sh --all --three-controllers  # ... and a third on :8092
#
# To reach the environment from other machines (e.g. on a server), run it as
# PUBLIC_BIND=0.0.0.0 ./setup.sh and add the hostnames on those machines too.
#
# PUBLIC_HOST=<host or IP> addresses the whole environment by that name
# instead of the *.test names, for a server people reach over the network
# without editing their hosts file:
#
#   PUBLIC_BIND=0.0.0.0 PUBLIC_HOST=10.0.0.5 ./setup.sh --all --standby
#
# It has to be one name everywhere, not a mixture: OIDC checks the issuer
# in the token against the one the controller was configured with, so the
# address a browser signs in at and the address the controller expects have
# to be the same. The *.test aliases keep working inside the compose
# network, which is what container-to-container traffic uses.
#
# Written for macOS bash 3.2 (no associative arrays).

set -euo pipefail
cd "$(dirname "$0")"
set -a; . ./.env; set +a

NODES="se dk"
PROFILE_ARGS=""
STANDBY=""
THIRD=""
for arg in "$@"; do
  case "$arg" in
    --three) NODES="se dk de"; PROFILE_ARGS="--profile three" ;;
    --all) NODES="se dk de uk us"; PROFILE_ARGS="--profile all" ;;
    --standby) STANDBY=1 ;;
    # Three is the number that makes a real installation's database
    # redundant (deploy/prod/README.md, section 12). Here the three share
    # one database container, so what it shows is the leadership order:
    # exactly one leads, the next in priority takes over, and the
    # preferred one takes it back.
    --three-controllers) STANDBY=1; THIRD=1 ;;
    *) echo "usage: $0 [--three|--all] [--standby|--three-controllers]" >&2; exit 2 ;;
  esac
done

# host_for <service-name> <port> gives the URL to use for a service:
# PUBLIC_HOST when it's set, its *.test alias otherwise.
url_of() {
  if [ -n "${PUBLIC_HOST:-}" ]; then echo "http://$PUBLIC_HOST:$2"; else echo "http://$1:$2"; fi
}
sceneid_url() { url_of sceneid.test 8080; }
forgejo_url() { url_of "forgejo-$1.test" "$(port_of "$1")"; }
controller_url() { url_of forgesync.test 8090; }
standby_url() { url_of forgesync-b.test 8091; }
third_url() { url_of forgesync-c.test 8092; }

compose() { docker compose $PROFILE_ARGS "$@"; }
port_of() { case "$1" in se) echo 3001;; dk) echo 3002;; de) echo 3003;; uk) echo 3004;; us) echo 3005;; esac; }
ssh_of()  { case "$1" in se) echo 2221;; dk) echo 2222;; de) echo 2223;; uk) echo 2224;; us) echo 2225;; esac; }
site_of() { echo "$1" | tr a-z A-Z; }
ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
FAILED=0

if [ -n "${PUBLIC_HOST:-}" ]; then
  echo "==> Addressing everything as $PUBLIC_HOST (PUBLIC_HOST is set)"
  ok "no /etc/hosts entries needed; the *.test aliases stay for container-to-container traffic"
else
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
fi

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
    # The discovery URL is the address people sign in at, so it changes
    # with PUBLIC_HOST. An existing source is updated rather than left
    # pointing at an address nobody can reach.
    src_id=$(fj admin auth list | awk '$2 == "SceneID" {print $1}')
    fj admin auth update-oauth --id "$src_id" \
      --auto-discover-url "$(sceneid_url)/realms/sceneid/.well-known/openid-configuration" >/dev/null
    compose restart "$svc" >/dev/null
    compose up -d --wait "$svc" >/dev/null
    ok "SceneID login source points at $(sceneid_url)"
  else
    fj admin auth add-oauth --name SceneID --provider openidConnect \
      --key forgejo --secret "$SCENEID_FORGEJO_CLIENT_SECRET" \
      --auto-discover-url "$(sceneid_url)/realms/sceneid/.well-known/openid-configuration" \
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

if [ -n "${PUBLIC_HOST:-}" ]; then
  # The realm is imported once, with the *.test callbacks; the addresses
  # people actually sign in at have to be allowed too, or SceneID refuses
  # the sign-in with "Invalid parameter: redirect_uri".
  kc_token=$(curl -fsS -X POST "$(sceneid_url)/realms/master/protocol/openid-connect/token" \
    -d "client_id=admin-cli" -d "username=$SCENEID_ADMIN_USER" -d "password=$SCENEID_ADMIN_PASSWORD" \
    -d "grant_type=password" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
  # The first "id" in the answer is the client's own; the later ones
  # belong to its protocol mappers, so take the first and stop.
  kc_client() { curl -fsS -H "Authorization: Bearer $kc_token" "$(sceneid_url)/admin/realms/sceneid/clients?clientId=$1" \
    | tr ',' '\n' | grep -m1 '"id"' | sed 's/.*"id":"\([^"]*\)".*/\1/'; }
  kc_redirects() {
    curl -fsS -X PUT -H "Authorization: Bearer $kc_token" -H "Content-Type: application/json" \
      -d "$2" "$(sceneid_url)/admin/realms/sceneid/clients/$1" >/dev/null
  }
  node_uris=""
  for n in $NODES; do
    node_uris="$node_uris\"$(forgejo_url "$n")/user/oauth2/SceneID/callback\","
  done
  kc_redirects "$(kc_client forgejo)" "{\"redirectUris\":[${node_uris%,}]}"
  ok "SceneID accepts the nodes' sign-in callbacks for $PUBLIC_HOST"
fi

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
    printf '  - name: %s\n    site: %s\n    url: %s\n    token_file: /etc/forgesync/tokens/%s.token\n    sceneid_source_id: %s\n' \
      "$n" "$(site_of "$n")" "$(forgejo_url "$n")" "$n" "${src:-0}"
  done
} > .work/forgesync.docker.yaml
if [ -n "${PUBLIC_HOST:-}" ]; then
  # Everything a browser sees has to agree: OIDC checks the issuer in the
  # token against the one the controller was configured with, so the
  # address people sign in at and the address here must be the same.
  sed -i.bak \
    -e "s|http://sceneid\.test:8080|$(sceneid_url)|g" \
    -e "s|http://forgesync\.test:8090|$(controller_url)|g" \
    .work/forgesync.docker.yaml
  rm -f .work/forgesync.docker.yaml.bak
fi
# The standby is the same config with its own name, port and URLs: it shares
# the database, so whichever holds the lease does the work.
sed -e 's|^  name: forgesync-a$|  name: forgesync-b|' \
    -e 's|^  priority: 1$|  priority: 2|' \
    -e "s|$(controller_url | sed 's|http://||')|$(standby_url | sed 's|http://||')|g" \
    -e 's|^  listen: 0\.0\.0\.0:8090$|  listen: 0.0.0.0:8091|' \
    .work/forgesync.docker.yaml > .work/forgesync-b.docker.yaml
docker compose $PROFILE_ARGS --profile controller up -d --build --force-recreate --wait forgesync
ok "controller running ($FORGESYNC_VERSION)"
if [ -n "$STANDBY" ]; then
  docker compose $PROFILE_ARGS --profile standby up -d --build --wait forgesync-b
  ok "standby controller running on :8091 (forgesync-b)"
fi
if [ -n "$THIRD" ]; then
  sed -e 's|^  name: forgesync-a$|  name: forgesync-c|' \
      -e 's|^  priority: 1$|  priority: 3|' \
      -e "s|$(controller_url | sed 's|http://||')|$(third_url | sed 's|http://||')|g" \
      -e 's|^  listen: 0\.0\.0\.0:8090$|  listen: 0.0.0.0:8092|' \
      .work/forgesync.docker.yaml > .work/forgesync-c.docker.yaml
  docker compose $PROFILE_ARGS --profile third up -d --build --wait forgesync-c
  ok "third controller running on :8092 (forgesync-c)"
fi

# ForgeSync's own accounts live in its database, which every controller
# share. The first one is made with the admin token, there being nobody to
# make it otherwise; after that they're managed in the UI.
admin_token=$(cat .tokens/admin.token)
if curl -fsS -H "Authorization: Bearer $admin_token" "$(controller_url)/api/v1/accounts" | grep -q '"total":0'; then
  curl -fsS -X POST -H "Authorization: Bearer $admin_token" -H "Content-Type: application/json" \
    -d "{\"username\":\"$FORGESYNC_ADMIN_USER\",\"password\":\"$FORGESYNC_ADMIN_PASSWORD\",\"role\":\"administrator\"}" \
    "$(controller_url)/api/v1/accounts" >/dev/null
  ok "ForgeSync account '$FORGESYNC_ADMIN_USER' created (password in .env)"
else
  ok "ForgeSync accounts exist"
fi

echo "==> Smoke tests"
disco="$(sceneid_url)/realms/sceneid/.well-known/openid-configuration"
if curl -fsS "$disco" | grep -q "\"issuer\":\"$(sceneid_url)/realms/sceneid\""; then
  ok "host -> SceneID discovery, issuer matches"
else
  fail "host -> SceneID discovery"
fi

for n in $NODES; do
  url="$(forgejo_url "$n")"
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

if curl -fsS "$(controller_url)/readyz" | grep -q ready; then
  ok "host -> ForgeSync controller ready"
else
  fail "ForgeSync controller not ready (docker compose --profile controller logs forgesync)"
fi
compose exec -T forgejo-se curl -fsS -o /dev/null "$(controller_url)/healthz" \
  && ok "forgejo-se -> ForgeSync controller reachable" || fail "forgejo-se -> ForgeSync controller"

echo
if [ "$FAILED" -ne 0 ]; then
  echo "Some checks failed; see above. Logs: docker compose logs <service>"
  exit 1
fi
cat <<EOF
Ready.
  SceneID admin console : $(sceneid_url)/admin   ($SCENEID_ADMIN_USER / $SCENEID_ADMIN_PASSWORD)
$(for n in $NODES; do echo "  Forgejo $(site_of "$n")            : $(forgejo_url "$n")   (ssh port $(ssh_of "$n"))"; done)
  SceneID test users    : alice / alice-pw, bob / bob-pw, carol / carol-pw, erin / erin-pw
                          (they sign in to the Forgejo nodes, not to ForgeSync)
  Local admins per node : siteadmin / $FORGEJO_SITEADMIN_PASSWORD, forgesync (API tokens in .tokens/)
  ForgeSync admin UI    : $(controller_url)   ($FORGESYNC_ADMIN_USER / $FORGESYNC_ADMIN_PASSWORD)
  Admin token           : .tokens/admin.token
  ForgeSync database    : localhost:5432 (forgesync / $FORGESYNC_DB_PASSWORD)
  Controller logs       : docker compose --profile controller logs -f forgesync
EOF
