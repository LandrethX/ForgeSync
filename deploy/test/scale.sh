#!/usr/bin/env bash
# Makes (or removes) a lot of repositories on one node, to measure what a
# scan round and a replication round cost when there are many.
#
#   ./scale.sh make 200        # 200 repositories owned by alice on SE
#   ./scale.sh drop            # remove every scale-* repository, everywhere
#
# What was measured on the LXC test server (five nodes, 203 repositories
# each, one commit apiece) is in CLAUDE.md; the short version is that the
# fast path stays a few seconds and the full round grows with
# repositories times nodes.
#
# Written for macOS bash 3.2, like the rest of the test environment.
set -Eeuo pipefail
cd "$(dirname "$0")"
set -a; . ./.env; set +a

NODE=${NODE:-se}
OWNER=${OWNER:-alice}
PUBLIC_HOST=${PUBLIC_HOST:-127.0.0.1}
port_of() { case "$1" in se) echo 3001;; dk) echo 3002;; de) echo 3003;; uk) echo 3004;; us) echo 3005;; esac; }
api() { # api <node> <method> <path> [body]
  local n=$1 method=$2 path=$3 body=${4:-}
  local args=(-s -o /dev/null -w '%{http_code}' -X "$method"
    -H "Authorization: token $(cat ".tokens/$n.token")" -H "Sudo: $OWNER")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}" "http://$PUBLIC_HOST:$(port_of "$n")$path"
}

case "${1:-}" in
  make)
    count=${2:-100}
    echo "Creating $count repositories as $OWNER on $NODE"
    i=1
    while [ "$i" -le "$count" ]; do
      name=$(printf 'scale-%04d' "$i")
      code=$(api "$NODE" POST /api/v1/user/repos \
        "{\"name\":\"$name\",\"auto_init\":true,\"default_branch\":\"main\",\"description\":\"scale test\"}")
      case "$code" in
        201|409) ;;
        *) echo "  $name: HTTP $code" ;;
      esac
      i=$((i + 1))
    done
    echo "Done. Ask for a scan and watch: docker compose logs -f forgesync | grep 'scan round'"
    ;;
  drop)
    for n in se dk de uk us; do
      [ -f ".tokens/$n.token" ] || continue
      names=$(curl -s -H "Authorization: token $(cat ".tokens/$n.token")" \
        "http://$PUBLIC_HOST:$(port_of "$n")/api/v1/repos/search?q=scale-&limit=200&uid=0" \
        | tr ',' '\n' | grep '"full_name"' | sed 's/.*"full_name":"\([^"]*\)".*/\1/' || true)
      for full in $names; do
        case "$full" in
          */scale-*) api "$n" DELETE "/api/v1/repos/$full" >/dev/null ;;
        esac
      done
      echo "  $n cleared"
    done
    ;;
  *)
    echo "usage: $0 make [count] | drop" >&2
    exit 2
    ;;
esac
