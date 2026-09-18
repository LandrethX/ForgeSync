#!/usr/bin/env bash
# P04 Replicating issues through the API: authors, timestamps, numbering.
#
# Questions:
#  - Does the Sudo header make the replicated issue/comment belong to the real user?
#  - Can non-admin tokens sudo? (Decides whether ForgeSync must be site admin.)
#  - Which timestamps can the API set? (Create options have no created_at; edits
#    and comments accept updated_at.)
#  - Do two nodes hand out the same issue number independently?
. "$(dirname "$0")/lib.sh"

NAME=p04-$RUN_ID
REPO=alice/$NAME
OLD=2020-01-02T03:04:05Z
for n in se dk; do
  for u in alice bob carol; do ensure_user $n $u; done
  create_repo $n alice "$NAME"
done

# ------------------------------------------------------------------------------------
section "Authorship with sudo"
iss=$(SUDO=bob must POST se "/repos/$REPO/issues" \
  "$(jq -nc --arg t "$OLD" '{title:"replicated issue", body:"created with sudo", created_at:$t}')")
hyp "An issue created with Sudo: bob is authored by bob" "$(is "$(printf '%s' "$iss" | jq -r .user.login)" bob)" \
  "author=$(printf '%s' "$iss" | jq -r .user.login)"
created=$(printf '%s' "$iss" | jq -r .created_at)
hyp "The issue's created_at can't be set (it gets the current time)" \
  "$( [ "${created#2020}" = "$created" ] && echo true || echo false)" "sent created_at=$OLD, got $created"

AS_TOKEN=$(user_token se alice) SUDO=bob api POST se "/repos/$REPO/issues" '{"title":"alice sudo bob"}' >/dev/null
hyp "Only site admins can use sudo (so ForgeSync needs a site-admin token)" \
  "$( [ "$(status)" = 403 ] && echo true || echo false)" "non-admin token + Sudo -> HTTP $(status) $(api_msg)"

# ------------------------------------------------------------------------------------
section "Timestamps"
num=$(printf '%s' "$iss" | jq -r .number)
ed=$(SUDO=bob api PATCH se "/repos/$REPO/issues/$num" "$(jq -nc --arg t "$OLD" '{title:"replicated issue (edited)", updated_at:$t}')")
hyp "An edit made with Sudo: bob can set updated_at" "$(is "$(printf '%s' "$ed" | jq -r .updated_at)" "$OLD")" \
  "HTTP $(status); updated_at=$(printf '%s' "$ed" | jq -r .updated_at)"
ed=$(api PATCH se "/repos/$REPO/issues/$num" "$(jq -nc --arg t "$OLD" '{body:"edited by forgesync itself", updated_at:$t}')")
hyp "An edit made as forgesync (admin, no sudo) can set updated_at" "$(is "$(printf '%s' "$ed" | jq -r .updated_at)" "$OLD")" \
  "HTTP $(status); updated_at=$(printf '%s' "$ed" | jq -r .updated_at). If only this works, keeping the author and the timestamp are mutually exclusive."

cm=$(SUDO=carol api POST se "/repos/$REPO/issues/$num/comments" "$(jq -nc --arg t "$OLD" '{body:"replicated comment", updated_at:$t}')")
info "Comment created with updated_at=$OLD" \
  "HTTP $(status); author=$(printf '%s' "$cm" | jq -r .user.login) created_at=$(printf '%s' "$cm" | jq -r .created_at) updated_at=$(printf '%s' "$cm" | jq -r .updated_at)"

# ------------------------------------------------------------------------------------
section "Issue numbers on two nodes"
a=$(SUDO=alice must POST dk "/repos/$REPO/issues" '{"title":"created on DK"}' | jq -r .number)
b=$(SUDO=bob   must POST se "/repos/$REPO/issues" '{"title":"created on SE at the same time"}' | jq -r .number)
info "Next issue number on each node" "DK gave #$a, SE gave #$b"
c=$(SUDO=alice must POST dk "/repos/$REPO/issues" '{"title":"second on DK"}' | jq -r .number)
hyp "Nodes number issues independently, so concurrent issues on two nodes can collide" \
  "$( [ "$a" = 1 ] && echo true || echo false)" \
  "SE already had issues up to #$b; DK started at #$a and continued with #$c. Any issue created on a replica takes a number the primary may also use."
