#!/usr/bin/env bash
# P05 Webhook coverage, signatures and sender identity.
#
# Forgejo v16's webhook event list has no events for labels, milestones, collaborators,
# settings, branch protection, reactions, users or teams. These checks confirm that in
# practice, showing which changes ForgeSync must find by polling.
#
# Also: are deliveries signed (HMAC) and who appears as the sender? Loop prevention is
# harder if ForgeSync's own sudo writes look like the user's.
. "$(dirname "$0")/lib.sh"

NAME=p05-$RUN_ID
REPO=alice/$NAME
for n in se dk; do
  for u in alice bob carol; do ensure_user $n $u; done
  create_repo $n alice "$NAME"
done
add_webhook se "$REPO" "$NAME-se"
add_webhook dk "$REPO" "$NAME-dk"
gitc clone -q "$(repo_url se forgesync "$(node_token se)" "$REPO")" "$WORK/r"

# observe TAG "change" yes|no ACTION...
# yes/no: whether Forgejo's event list suggests the change sends a webhook at all.
observe() {
  local tag=$1 what=$2 expected=$3; shift 3
  local seq got summary sent
  seq=$(hooks_seq)
  "$@" >/dev/null
  got=$(hooks_after "$seq" "$tag")
  summary=$(printf '%s' "$got" | jq -r 'if length == 0 then "nothing" else
    [.[] | "\(.event_type // .event)\(if .action then "/" + .action else "" end) by \(.sender // .pusher // "?")"] | join(", ") end')
  sent=$(printf '%s' "$got" | jq -r 'if length > 0 then "yes" else "no" end')
  if [ "$expected" = yes ]; then
    hyp "$what sends a webhook" "$(is "$sent" yes)" "received: $summary"
  else
    hyp "$what sends no webhook (must be found by polling)" "$(is "$sent" no)" "received: $summary"
  fi
}

push_commit() { new_commit "$WORK/r" "$1" && gitc -C "$WORK/r" push -q origin HEAD:main; }
issue_no() { api GET se "/repos/$REPO/issues?state=all&type=issues" | jq -r '.[0].number'; }

# ------------------------------------------------------------------------------------
section "Which changes send webhooks (SE)"
T=$NAME-se
observe $T "git push"                  yes push_commit "pushed"
observe $T "create branch (API)"       yes must POST se "/repos/$REPO/branches" '{"new_branch_name":"feature","old_ref_name":"main"}'
observe $T "delete branch (API)"       yes must DELETE se "/repos/$REPO/branches/feature"
create_issue() { SUDO=alice must POST se "/repos/$REPO/issues" '{"title":"hooked issue"}'; }
observe $T "create issue (sudo alice)" yes create_issue
I=$(issue_no)
observe $T "edit issue title"          yes must PATCH se "/repos/$REPO/issues/$I" '{"title":"hooked issue (edited)"}'
observe $T "comment on issue"          yes must POST se "/repos/$REPO/issues/$I/comments" '{"body":"a comment"}'
C=$(api GET se "/repos/$REPO/issues/$I/comments" | jq -r '.[0].id')
observe $T "edit comment"              yes must PATCH se "/repos/$REPO/issues/comments/$C" '{"body":"edited comment"}'
observe $T "create repo label"         no  must POST se "/repos/$REPO/labels" '{"name":"bug","color":"#ee0701"}'
L=$(api GET se "/repos/$REPO/labels" | jq -r '.[0].id')
observe $T "edit repo label"           no  must PATCH se "/repos/$REPO/labels/$L" '{"color":"#00aa00"}'
observe $T "add label to issue"        yes must POST se "/repos/$REPO/issues/$I/labels" "{\"labels\":[$L]}"
observe $T "create milestone"          no  must POST se "/repos/$REPO/milestones" '{"title":"v1"}'
M=$(api GET se "/repos/$REPO/milestones" | jq -r '.[0].id')
observe $T "edit milestone"            no  must PATCH se "/repos/$REPO/milestones/$M" '{"title":"v1.0"}'
observe $T "set issue milestone"       yes must PATCH se "/repos/$REPO/issues/$I" "{\"milestone\":$M}"
observe $T "add reaction"              no  must POST se "/repos/$REPO/issues/$I/reactions" '{"content":"+1"}'
observe $T "close issue"               yes must PATCH se "/repos/$REPO/issues/$I" '{"state":"closed"}'
observe $T "create release"            yes must POST se "/repos/$REPO/releases" '{"tag_name":"v1.0","name":"v1.0","target_commitish":"main"}'
observe $T "create wiki page"          yes must POST se "/repos/$REPO/wiki/new" "{\"title\":\"Home\",\"content_base64\":\"$(printf 'hi' | base64)\"}"
observe $T "change repo description"   no  must PATCH se "/repos/$REPO" '{"description":"changed"}'
observe $T "add collaborator"          no  must PUT se "/repos/$REPO/collaborators/bob" '{"permission":"write"}'
observe $T "add branch protection"     no  must POST se "/repos/$REPO/branch_protections" '{"rule_name":"main"}'

valid=$(curl -fsS "$HOOKSINK/events" | jq -r --arg t "/hook/$T" '[.[] | select(.path == $t)] | (map(select(.signature_valid)) | length|tostring) + "/" + (length|tostring)')
headers=$(curl -fsS "$HOOKSINK/events" | jq -r --arg t "/hook/$T" '[.[] | select(.path == $t) | .signature_headers[]] | unique | join(", ")')
hyp "Every delivery carries a valid HMAC-SHA256 signature" "$(printf '%s' "$valid" | awk -F/ '{print ($1 == $2 && $2 > 0) ? "true" : "false"}')" \
  "$valid valid; headers: $headers"

# ------------------------------------------------------------------------------------
section "Sender identity for ForgeSync's own writes (DK)"
T=$NAME-dk
gitc clone -q "$(repo_url dk forgesync "$(node_token dk)" "$REPO")" "$WORK/d"
seq=$(hooks_seq)
new_commit "$WORK/d" "replicated push" && gitc -C "$WORK/d" push -q origin HEAD:main
got=$(hooks_after "$seq" "$T")
pusher=$(printf '%s' "$got" | jq -r '[.[] | select(.event == "push") | .pusher] | first // ""')
hyp "A replicated git push shows forgesync as the pusher (so loop prevention can filter it)" "$(is "$pusher" forgesync)" "pusher=$pusher"

seq=$(hooks_seq)
SUDO=bob must POST dk "/repos/$REPO/issues" '{"title":"replicated with sudo"}' >/dev/null
got=$(hooks_after "$seq" "$T")
sender=$(printf '%s' "$got" | jq -r '[.[] | select(.event == "issues") | .sender] | first // ""')
hyp "An issue replicated with Sudo: bob shows bob as the sender, so it can't be told apart from bob's own edits" \
  "$(is "$sender" bob)" "sender=$sender. Loop prevention then needs an expected-change record, not a sender filter."
