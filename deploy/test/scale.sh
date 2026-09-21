#!/usr/bin/env bash
# Makes (or removes) a lot of repositories on one node, to measure what a
# scan round and a replication round cost when there are many.
#
#   ./scale.sh make 200        # 200 repositories owned by alice on SE
#   ./scale.sh drop            # remove every scale-* repository, everywhere
#
# What was measured on the LXC test server (five nodes, 203 repositories
# each, one commit apiece) is in docs/PERFORMANCE.md; the short version is that the
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

# admin_api is the same call made as the service account rather than as
# $OWNER. ForgeSync's archive organization is private and owned by the
# service account, so a Sudo call as the repository's owner cannot see it
# and answers 404 for everything in it.
admin_api() { # admin_api <node> <method> <path>
  local n=$1 method=$2 path=$3
  curl -s -o /dev/null -w '%{http_code}' -X "$method" \
    -H "Authorization: token $(cat ".tokens/$n.token")" \
    "http://$PUBLIC_HOST:$(port_of "$n")$path"
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
    # Two passes. First the repositories themselves, on whichever node has
    # them. Forgejo caps a search response at MAX_RESPONSE_ITEMS (50 by
    # default) however large a limit is asked for, so this keeps asking
    # until a page yields nothing. Deleting one page and saying "cleared"
    # is what it used to do, which left 950 of 1000 behind and reported
    # success.
    #
    # Both passes count what actually went, not what was attempted: a
    # delete that is refused must end the loop rather than make it ask for
    # the same page for ever.
    clear_matching() { # clear_matching <node> <shell pattern> <admin?>
      local n=$1 pattern=$2 admin=$3 gone=0 page names full code
      while :; do
        names=$(curl -s -H "Authorization: token $(cat ".tokens/$n.token")" \
          "http://$PUBLIC_HOST:$(port_of "$n")/api/v1/repos/search?q=scale-&limit=50&uid=0" \
          | tr ',' '\n' | { grep '"full_name"' || true; } \
          | sed 's/.*"full_name":"\([^"]*\)".*/\1/')
        [ -n "$names" ] || break
        page=0
        for full in $names; do
          # shellcheck disable=SC2254  # the pattern is ours, and is meant to glob
          case "$full" in
            $pattern) ;;
            *) continue ;;
          esac
          if [ "$admin" = admin ]; then
            code=$(admin_api "$n" DELETE "/api/v1/repos/$full")
          else
            code=$(api "$n" DELETE "/api/v1/repos/$full")
          fi
          case "$code" in
            20*) gone=$((gone + 1)); page=$((page + 1)) ;;
            *) printf '  %s: %s refused the delete (%s)\n' "$n" "$full" "$code" >&2 ;;
          esac
        done
        [ "$page" -gt 0 ] || break
      done
      printf '%s' "$gone"
    }

    for n in se dk de uk us; do
      [ -f ".tokens/$n.token" ] || continue
      gone=$(clear_matching "$n" '*/scale-*' owner)
      printf '  %s cleared, %s deleted\n' "$n" "$gone"
    done

    # Deleting a repository on its primary archives the copies rather than
    # deleting them: each is renamed <owner>--<name>--<time> and moved into
    # replication.archive_org, to be purged after backup_days. That is the
    # right thing for real work and only clutter after a scale run, so take
    # the archives of scale- repositories away too. They belong to the
    # service account, so these deletes are not sudoed.
    for n in se dk de uk us; do
      [ -f ".tokens/$n.token" ] || continue
      gone=$(clear_matching "$n" 'forgesync-archive/*--scale-*' admin)
      [ "$gone" = 0 ] || printf '  %s archives cleared, %s deleted\n' "$n" "$gone"
    done
    ;;
  *)
    echo "usage: $0 make [count] | drop" >&2
    exit 2
    ;;
esac
