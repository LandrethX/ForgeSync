#!/usr/bin/env bash
# Checks conflict dismissal against a running environment, end to end:
# make a conflict ForgeSync can't settle, dismiss it, see that it stays
# dismissed, see that it comes back when what it says changes, and see
# that it clears when the difference goes away.
#
#   ./check-dismissal.sh                      # the .test names (a Mac)
#   PUBLIC_HOST=192.0.2.10 ./check-dismissal.sh
#
# It uses an Actions secret, which is the honest case: Forgejo never
# gives a secret's value back, so ForgeSync can report the difference and
# can never fix it. Nothing else is touched, and the secret is removed
# again at the end.
#
# Written for macOS bash 3.2, like the rest of the test environment.
set -Eeuo pipefail
cd "$(dirname "$0")"

PUBLIC_HOST=${PUBLIC_HOST:-127.0.0.1}
SECRET=${SECRET:-DISMISSAL_CHECK}
CONTROLLER=${CONTROLLER:-http://$PUBLIC_HOST:8090}
TOKEN=$(cat .tokens/admin.token)
FAILED=0

ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
port_of() { case "$1" in se) echo 3001;; dk) echo 3002;; de) echo 3003;; uk) echo 3004;; us) echo 3005;; esac; }
node_url() { echo "http://$PUBLIC_HOST:$(port_of "$1")"; }
fs() { # fs <method> <path> [body]
  local method=$1 path=$2 body=${3:-}
  if [ -n "$body" ]; then
    curl -fsS -X "$method" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
      -d "$body" "$CONTROLLER/api/v1$path"
  else
    curl -fsS -X "$method" -H "Authorization: Bearer $TOKEN" "$CONTROLLER/api/v1$path"
  fi
}
# python3 reads the answers: every test environment has it, and it beats
# guessing at JSON with sed.
jq3() { python3 -c "$1"; }

candidates() { # every replicated repository: name, primary, the nodes that have it
  fs GET "/repositories?limit=50" | jq3 "
import json,sys
for r in json.load(sys.stdin)['items']:
    if r['primary_node'] and r['status'] != 'deleted':
        print(r['full_name'], r['primary_node'], ' '.join(n['node'] for n in r['nodes'] if n['presence'] == 'present'))"
}

has_actions() { # Actions can be off for a repository on a node, and then
                # its endpoints answer 404: nothing to make a conflict of.
  local repo=$1 node=$2
  [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: token $(cat ".tokens/$node.token")" \
    "$(node_url "$node")/api/v1/repos/$repo/actions/variables")" = 200 ]
}

round() { # ask for a scan round and wait for it to finish
  fs POST /inventory/scan >/dev/null
  local i=0
  while [ $i -lt 90 ]; do
    if fs GET /inventory | grep -q '"running":false'; then return 0; fi
    sleep 2; i=$((i + 2))
  done
  fail "the scan round didn't finish within 90s"
}

conflict_state() { # the state of our conflict, or "none"
  fs GET "/conflicts?state=all&limit=100" | jq3 "
import json,sys
for c in json.load(sys.stdin)['items']:
    if c['kind'] == 'actions_secret_missing' and c['ref'] == '$SECRET':
        print(c['state'], c['id']); break
else: print('none 0')"
}

secret_on() { # secret_on <node> <set|delete>
  local n=$1 action=$2 code
  if [ "$action" = set ]; then
    code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: token $(cat ".tokens/$n.token")" \
      -H 'Content-Type: application/json' -d '{"data":"only-a-check"}' \
      "$(node_url "$n")/api/v1/repos/$REPO/actions/secrets/$SECRET")
  else
    code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: token $(cat ".tokens/$n.token")" \
      "$(node_url "$n")/api/v1/repos/$REPO/actions/secrets/$SECRET")
  fi
  case "$code" in 20*|404) ;; *) fail "$action $SECRET on $n: HTTP $code" ;; esac
}

REPO=""
while read -r repo primary others; do
  [ -n "$repo" ] || continue
  if has_actions "$repo" "$primary"; then
    REPO=$repo PRIMARY=$primary OTHERS=$others
    break
  fi
done <<EOT
$(candidates)
EOT
[ -n "$REPO" ] || { echo "no replicated repository with Actions turned on to test with" >&2; exit 1; }
SECOND=$(for n in $OTHERS; do [ "$n" != "$PRIMARY" ] && echo "$n" && break; done)
echo "==> $REPO, primary $PRIMARY, also on: $OTHERS"
cleanup() { for n in $OTHERS; do secret_on "$n" delete; done; }
trap cleanup EXIT

echo "==> A conflict ForgeSync can't settle"
secret_on "$PRIMARY" set
round
read -r state id <<<"$(conflict_state)"
[ "$state" = open ] && ok "the missing secret is an open conflict (#$id)" || fail "state is $state, wanted open"

echo "==> Dismissing it"
fs POST "/conflicts/$id/dismiss" '{"note":"a check, not a real difference"}' >/dev/null
read -r state _ <<<"$(conflict_state)"
[ "$state" = dismissed ] && ok "it's dismissed" || fail "state is $state, wanted dismissed"
open_now=$(fs GET /overview | jq3 "import json,sys; print(json.load(sys.stdin)['open_conflicts'])")
ok "the dashboard counts $open_now open conflicts"

echo "==> It stays dismissed when nothing changes"
round
read -r state _ <<<"$(conflict_state)"
[ "$state" = dismissed ] && ok "still dismissed after a fresh round" || fail "state is $state after a round"

echo "==> It comes back when what it says changes"
secret_on "$SECOND" set   # one fewer node is missing it now
round
read -r state _ <<<"$(conflict_state)"
[ "$state" = open ] && ok "open again, because the difference is a different one" || fail "state is $state, wanted open"

echo "==> It clears when the difference goes"
for n in $OTHERS; do secret_on "$n" set; done
round
read -r state _ <<<"$(conflict_state)"
[ "$state" = cleared ] && ok "cleared on its own" || fail "state is $state, wanted cleared"

trap - EXIT
cleanup
echo
if [ "$FAILED" = 0 ]; then echo "All good."; else echo "Something's wrong (above)."; exit 1; fi
