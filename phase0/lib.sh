# Shared helpers for the Phase 0 probes. Source this; don't run it.
# Written for macOS bash 3.2. Needs curl, jq, git and docker on the Mac.

set -Eeuo pipefail

PHASE0_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TEST_DIR=$(cd "$PHASE0_DIR/../deploy/test" && pwd)
set -a; . "$TEST_DIR/.env"; set +a

SCENEID=http://sceneid.test:8080
REALM_URL=$SCENEID/realms/sceneid
HOOKSINK=http://localhost:8099
HOOKSINK_INTERNAL=http://hooksink:8099

# RUN_ID suffixes every repo and user a probe creates, so re-runs never collide.
RUN_ID=${RUN_ID:-$(date +%m%d%H%M%S)}
RESULTS=${RESULTS:-$PHASE0_DIR/results/run-$RUN_ID.tsv}
CACHE=$PHASE0_DIR/.cache
mkdir -p "$(dirname "$RESULTS")" "$CACHE"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
trap 'record ERROR "script error (line $LINENO): $BASH_COMMAND" "see output above"' ERR

export GIT_TERMINAL_PROMPT=0

for tool in curl jq git docker ssh-keygen; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }
done

# ---------------------------------------------------------------- results

PROBE=${PROBE:-$(basename "$0" .sh)}

# record VERDICT CHECK [DETAIL]
# VERDICT: CONFIRMED | REFUTED (hypothesis held / did not) | INFO (fact) | ERROR
record() {
  local verdict=$1 check=$2 detail=${3:-} colour
  detail=$(printf '%s' "$detail" | tr '\t\n' '  ' | cut -c1-400)
  printf '%s\t%s\t%s\t%s\t%s\n' "$RUN_ID" "$PROBE" "$verdict" "$check" "$detail" >> "$RESULTS"
  case $verdict in CONFIRMED) colour=32;; REFUTED) colour=33;; ERROR) colour=31;; *) colour=36;; esac
  printf '  \033[%sm%-9s\033[0m %s\n' "$colour" "$verdict" "$check"
  [ -n "$detail" ] && printf '            %s\n' "$detail"
  return 0
}

# hyp HYPOTHESIS true|false [DETAIL]
hyp() { if [ "$2" = true ]; then record CONFIRMED "$1" "${3:-}"; else record REFUTED "$1" "${3:-}"; fi; }
info() { record INFO "$1" "${2:-}"; }
section() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
is() { if [ "$1" = "$2" ]; then echo true; else echo false; fi; }

# ---------------------------------------------------------------- nodes

# The probes are written for two roles, "se" (node A, where things happen first)
# and "dk" (node B, the replica). NODE_A / NODE_B pick the real nodes, so the
# same probes run against any pair: NODE_A=de NODE_B=uk ./run-all.sh
NODE_A=${NODE_A:-se}
NODE_B=${NODE_B:-dk}
real_node() { case $1 in se) echo "$NODE_A";; dk) echo "$NODE_B";; *) echo "$1";; esac; }

node_url() {
  case $(real_node "$1") in
    se) echo http://forgejo-se.test:3001;;
    dk) echo http://forgejo-dk.test:3002;;
    de) echo http://forgejo-de.test:3003;;
    uk) echo http://forgejo-uk.test:3004;;
    us) echo http://forgejo-us.test:3005;;
    *) echo "unknown node $(real_node "$1")" >&2; return 1;;
  esac
}
node_host() { node_url "$1" | sed 's|^http://||'; }
node_token() { cat "$TEST_DIR/.tokens/$(real_node "$1").token"; }

# fj NODE ARGS...  run the forgejo CLI on a node as the git user
fj() {
  local node; node=$(real_node "$1"); shift
  (cd "$TEST_DIR" && docker compose --profile all exec -T -u git "forgejo-$node" forgejo "$@")
}

sceneid_source_id() { fj "$1" admin auth list | awk '$2 == "SceneID" {print $1}'; }

# ---------------------------------------------------------------- Forgejo API

# api METHOD NODE PATH [JSON]  prints the body; the HTTP status goes to $(status).
# Env: AS_TOKEN overrides the forgesync token; SUDO=<user> sends the Sudo header.
api() {
  local method=$1 node=$2 path=$3 data=${4:-}
  local args=(-sS -X "$method" -o "$WORK/body" -w '%{http_code}'
              -H "Authorization: token ${AS_TOKEN:-$(node_token "$node")}"
              -H 'Accept: application/json')
  if [ -n "${SUDO:-}" ]; then args+=(-H "Sudo: $SUDO"); fi
  if [ -n "$data" ]; then args+=(-H 'Content-Type: application/json' --data "$data"); fi
  curl "${args[@]}" "$(node_url "$node")/api/v1$path" > "$WORK/status" || echo 000 > "$WORK/status"
  cat "$WORK/body"
}
status() { cat "$WORK/status"; }
ok_status() { case $(status) in 2??) echo true;; *) echo false;; esac; }
api_msg() { jq -r '.message // empty' "$WORK/body" 2>/dev/null | head -c 200; }

# must METHOD NODE PATH [JSON]  like api, but aborts the probe on a non-2xx status
must() {
  local out; out=$(api "$@")
  if [ "$(ok_status)" != true ]; then
    echo "API $1 $2 $3 -> $(status): $out" >&2
    return 1
  fi
  printf '%s' "$out"
}

# create_repo NODE OWNER NAME  repo with an initial commit on main
create_repo() {
  must POST "$1" "/admin/users/$2/repos" \
    "$(jq -nc --arg n "$3" '{name:$n, auto_init:true, readme:"Default", default_branch:"main"}')" >/dev/null
}

# admin_users NODE  every user on the node (admin view, all pages) as one JSON array
admin_users() {
  local page=1 all='[]' chunk
  while :; do
    chunk=$(must GET "$1" "/admin/users?limit=50&page=$page")
    [ "$(printf '%s' "$chunk" | jq length)" -eq 0 ] && break
    all=$(jq -nc --argjson a "$all" --argjson b "$chunk" '$a + $b')
    page=$((page + 1))
  done
  printf '%s' "$all"
}

branch_sha() { api GET "$1" "/repos/$2/branches/$3" | jq -r '.commit.id // empty'; }

# user_token NODE USER  API token for a SceneID user, generated with the CLI (cached)
user_token() {
  local f; f="$CACHE/token-$(real_node "$1")-$2"
  # Tokens go stale when the test environment is reset; check before reuse.
  if [ -s "$f" ] && [ "$(AS_TOKEN=$(cat "$f") api GET "$1" /user | jq -r '.login // empty')" != "$2" ]; then
    rm -f "$f"
  fi
  if [ ! -s "$f" ]; then
    fj "$1" admin user generate-access-token --username "$2" \
      --token-name "phase0-$(date +%s)-$RANDOM" --scopes all --raw > "$f"
  fi
  cat "$f"
}

user_exists() { api GET "$1" "/users/$2" >/dev/null; [ "$(status)" = 200 ]; }

# ensure_user NODE USER  make sure a SceneID test user exists on the node (by logging in)
ensure_user() {
  user_exists "$1" "$2" && return 0
  local final; final=$(sceneid_login "$1" "$2" "$2-pw" "$WORK/jar-ensure-$1-$2")
  user_exists "$1" "$2" || { echo "could not create $2 on $1 via SceneID login (ended at $final)" >&2; return 1; }
}

# ---------------------------------------------------------------- git

gitc() {
  git -c credential.helper= -c user.name="Phase0 Probe" -c user.email=probe@forgesync.test \
      -c init.defaultBranch=main -c advice.detachedHead=false "$@"
}

# repo_url NODE USER TOKEN OWNER/REPO
repo_url() { echo "http://$2:$3@$(node_host "$1")/$4.git"; }

# try_git DIR ARGS...  runs git, sets GIT_RC and GIT_OUT, never aborts the probe
try_git() {
  local dir=$1; shift
  if GIT_OUT=$(cd "$dir" && gitc "$@" 2>&1); then GIT_RC=0; else GIT_RC=$?; fi
  GIT_OUT=$(printf '%s' "$GIT_OUT" | { grep -E 'remote:|error:|rejected|->|fatal:' || true; } | { grep -v '^remote: *$' || true; } | head -4 | tr '\n' ' ' | sed 's/http:\/\/[^@]*@/http:\/\/***@/g')
}

ssh_port() { case $(real_node "$1") in se) echo 2221;; dk) echo 2222;; de) echo 2223;; uk) echo 2224;; us) echo 2225;; esac; }
ssh_url() { echo "ssh://git@$(node_host "$1" | cut -d: -f1):$(ssh_port "$1")/$2.git"; }

# new_key NAME  creates an ed25519 key pair in $WORK; prints the private key path
new_key() { ssh-keygen -q -t ed25519 -N '' -C "$1@phase0" -f "$WORK/$1" && echo "$WORK/$1"; }

# try_ssh_git KEY DIR ARGS...  like try_git, authenticating over SSH with KEY only
try_ssh_git() {
  local key=$1; shift
  GIT_SSH_COMMAND="ssh -i $key -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes"
  export GIT_SSH_COMMAND
  try_git "$@"
  unset GIT_SSH_COMMAND
}

new_commit() { (cd "$1" && echo "$2 $RANDOM$RANDOM" >> probe.txt && gitc add probe.txt && gitc commit -qm "$2"); }

# ---------------------------------------------------------------- SceneID (Keycloak)

kc_admin_token() {
  curl -fsS "$REALM_URL/protocol/openid-connect/token" \
    -d grant_type=client_credentials -d client_id=forgesync \
    -d client_secret="$SCENEID_FORGESYNC_CLIENT_SECRET" | jq -r .access_token
}

# kc_create_user USERNAME  creates a user with password USERNAME-pw; prints the sub
kc_create_user() {
  local tok; tok=$(kc_admin_token)
  curl -fsS -o /dev/null -X POST "$SCENEID/admin/realms/sceneid/users" \
    -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
    --data "$(jq -nc --arg u "$1" '{username:$u, enabled:true, email:($u+"@sceneid.test"),
      emailVerified:true, firstName:"Probe", lastName:$u,
      credentials:[{type:"password", value:($u+"-pw"), temporary:false}]}')"
  kc_user_id "$1"
}

kc_user_id() {
  curl -fsS -H "Authorization: Bearer $(kc_admin_token)" \
    "$SCENEID/admin/realms/sceneid/users?username=$1&exact=true" | jq -r '.[0].id'
}

# kc_master_token  token for the Keycloak bootstrap admin (only for realm config changes
# that the forgesync client isn't allowed to make)
kc_master_token() {
  curl -fsS "$SCENEID/realms/master/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=admin-cli \
    -d username="$SCENEID_ADMIN_USER" -d password="$SCENEID_ADMIN_PASSWORD" | jq -r .access_token
}

# kc_user_update USERNAME JSON  merges JSON into the user's representation (forgesync client)
kc_user_update() {
  local tok id cur
  tok=$(kc_admin_token); id=$(kc_user_id "$1")
  cur=$(curl -fsS -H "Authorization: Bearer $tok" "$SCENEID/admin/realms/sceneid/users/$id")
  curl -sS -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
    --data "$(jq -nc --argjson a "$cur" --argjson b "$2" '$a * $b')" "$SCENEID/admin/realms/sceneid/users/$id"
}

# sceneid_login NODE USER PASSWORD JAR
# Drives the browser login flow with curl. Prints the URL Forgejo finally lands
# on; the Forgejo session cookie stays in JAR.
sceneid_login() {
  local base; base=$(node_url "$1")
  local page action
  rm -f "$4"
  page=$(curl -sS -L -c "$4" -b "$4" "$base/user/oauth2/SceneID")
  action=$(printf '%s' "$page" | tr '\n' ' ' \
    | sed -n 's/.*action="\([^"]*login-actions\/authenticate[^"]*\)".*/\1/p' | sed 's/&amp;/\&/g')
  if [ -z "$action" ]; then
    echo "no SceneID login form found (page title: $(printf '%s' "$page" | sed -n 's/.*<title>\(.*\)<\/title>.*/\1/p' | head -1))" >&2
    return 1
  fi
  curl -sS -L -c "$4" -b "$4" -o "$WORK/login-final.html" -w '%{url_effective}' \
    --data-urlencode "username=$2" --data-urlencode "password=$3" --data credentialId= "$action"
}

# session_login JAR NODE  login name of the Forgejo web session in JAR (empty if none).
# Forgejo's API doesn't accept session cookies ("token is required"), so this
# reads the settings page, which redirects to the login page without a session.
# A prohibit_login user gets an "Account is suspended" page instead, which still
# shows "Signed in as" in the navbar, so that counts as no session too.
session_login() {
  local out; out=$(curl -sS -L -b "$1" -w '\n%{url_effective}' "$(node_url "$2")/user/settings" 2>/dev/null || true)
  case $(printf '%s' "$out" | tail -n 1) in */user/login*) return 0;; esac
  case $out in *"<title>Account is suspended"*) return 0;; esac
  printf '%s' "$out" | sed -n 's/.*Signed in as <strong>\([^<]*\)<.*/\1/p' | head -n 1
}

# ---------------------------------------------------------------- webhooks

hooks_seq() { curl -fsS "$HOOKSINK/events" | jq 'map(.seq) | max // 0'; }

# hooks_after SEQ TAG  waits until deliveries for TAG settle, prints them as a JSON array
hooks_after() {
  local last=-1 now i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    sleep 1
    now=$(curl -fsS "$HOOKSINK/events?after=$1" | jq --arg t "/hook/$2" '[.[] | select(.path == $t)] | length')
    [ "$now" -gt 0 ] && [ "$now" = "$last" ] && break
    last=$now
  done
  curl -fsS "$HOOKSINK/events?after=$1" | jq -c --arg t "/hook/$2" '[.[] | select(.path == $t)]'
}

# add_webhook NODE OWNER/REPO TAG
add_webhook() {
  local events='["create","delete","fork","push","issues","issue_assign","issue_label","issue_milestone",
    "issue_comment","pull_request","pull_request_assign","pull_request_label","pull_request_milestone",
    "pull_request_comment","pull_request_review_approved","pull_request_review_rejected",
    "pull_request_review_comment","pull_request_sync","pull_request_review_request","wiki",
    "repository","release","package"]'
  must POST "$1" "/repos/$2/hooks" "$(jq -nc --arg u "$HOOKSINK_INTERNAL/hook/$3" --arg s "$HOOK_SECRET" \
    --argjson e "$events" '{type:"forgejo", active:true, branch_filter:"*", events:$e,
      config:{url:$u, content_type:"json", secret:$s}}')" >/dev/null
}

printf '\033[1m## %s (run %s)\033[0m\n' "$PROBE" "$RUN_ID"
