#!/usr/bin/env bash
# P02 Read-only replicas using stock branch and tag protection.
#
# Idea from the review: on a replica node, one "*" branch rule and one "*" tag rule
# whose push allow-list holds only the forgesync account. Users can't write Git there;
# ForgeSync can. No hooks and no Forgejo changes, and it stays in force while ForgeSync
# is down.
#
# Questions:
#  - Does the rule block the repository owner, with apply_to_admins off and on?
#  - Are new branches, tags, the web/API editor and the branch API blocked too?
#  - Can ForgeSync still push, and does a push with an expected old value (lease) fail safely?
#  - Can the owner simply remove the rule?
. "$(dirname "$0")/lib.sh"

N=dk
REPO=alice/p02-$RUN_ID
ensure_user $N alice
create_repo $N alice "p02-$RUN_ID"
ALICE_TOK=$(user_token $N alice)
FS_TOK=$(node_token $N)
URL_ALICE=$(repo_url $N alice "$ALICE_TOK" "$REPO")
URL_FS=$(repo_url $N forgesync "$FS_TOK" "$REPO")

gitc clone -q "$URL_FS" "$WORK/r"

set_branch_rule() {  # set_branch_rule true|false  (apply_to_admins)
  api DELETE $N "/repos/$REPO/branch_protections/%2A" >/dev/null
  must POST $N "/repos/$REPO/branch_protections" "$(jq -nc --argjson a "$1" \
    '{rule_name:"*", enable_push:true, enable_push_whitelist:true,
      push_whitelist_usernames:["forgesync"], apply_to_admins:$a}')" >/dev/null
}

# ------------------------------------------------------------------------------------
section "Before protection"
new_commit "$WORK/r" "alice before protection"
try_git "$WORK/r" push "$URL_ALICE" HEAD:refs/heads/main
hyp "Owner can push while the repo is unprotected (sanity check)" "$(is $GIT_RC 0)" "$GIT_OUT"

# ------------------------------------------------------------------------------------
section "Rule '*' with push allow-list [forgesync], apply_to_admins=false"
set_branch_rule false
new_commit "$WORK/r" "alice with apply_to_admins=false"
try_git "$WORK/r" push "$URL_ALICE" HEAD:refs/heads/main
hyp "Owner's push to main is rejected (apply_to_admins=false)" "$( [ $GIT_RC -ne 0 ] && echo true || echo false)" "$GIT_OUT"

# ------------------------------------------------------------------------------------
section "Rule '*' with push allow-list [forgesync], apply_to_admins=true, plus tag rule '*'"
set_branch_rule true
must POST $N "/repos/$REPO/tag_protections" \
  '{"name_pattern":"*","whitelist_usernames":["forgesync"]}' >/dev/null
gitc -C "$WORK/r" fetch -q "$URL_FS" main && gitc -C "$WORK/r" reset -q --hard FETCH_HEAD

new_commit "$WORK/r" "alice to main"
try_git "$WORK/r" push "$URL_ALICE" HEAD:refs/heads/main
hyp "Owner's push to main is rejected" "$( [ $GIT_RC -ne 0 ] && echo true || echo false)" "$GIT_OUT"

try_git "$WORK/r" push "$URL_ALICE" HEAD:refs/heads/alice-feature
hyp "Owner can't create a new branch by pushing" "$( [ $GIT_RC -ne 0 ] && echo true || echo false)" "$GIT_OUT"

gitc -C "$WORK/r" tag -f alice-tag >/dev/null
try_git "$WORK/r" push "$URL_ALICE" refs/tags/alice-tag
hyp "Owner can't push a tag" "$( [ $GIT_RC -ne 0 ] && echo true || echo false)" "$GIT_OUT"

AS_TOKEN=$ALICE_TOK api POST $N "/repos/$REPO/contents/alice.txt" \
  "$(jq -nc --arg c "$(printf 'hello' | base64)" '{content:$c, message:"alice via API", branch:"main"}')" >/dev/null
hyp "Owner can't commit through the contents API (web editor path)" "$( [ "$(ok_status)" = false ] && echo true || echo false)" "HTTP $(status) $(api_msg)"

AS_TOKEN=$ALICE_TOK api POST $N "/repos/$REPO/branches" '{"new_branch_name":"alice-api","old_ref_name":"main"}' >/dev/null
hyp "Owner can't create a branch through the API" "$( [ "$(ok_status)" = false ] && echo true || echo false)" "HTTP $(status) $(api_msg)"

AS_TOKEN=$ALICE_TOK api POST $N "/repos/$REPO/issues" '{"title":"issue on a protected replica"}' >/dev/null
info "Branch protection doesn't restrict issues (owner creating an issue)" "HTTP $(status) - issues need their own policy"

# ------------------------------------------------------------------------------------
section "ForgeSync writes"
gitc -C "$WORK/r" fetch -q "$URL_FS" main && gitc -C "$WORK/r" reset -q --hard FETCH_HEAD
old_sha=$(gitc -C "$WORK/r" rev-parse HEAD)
new_commit "$WORK/r" "forgesync replicates"
try_git "$WORK/r" push "$URL_FS" HEAD:refs/heads/main
hyp "ForgeSync can push to main" "$(is $GIT_RC 0)" "$GIT_OUT"

try_git "$WORK/r" push "$URL_FS" HEAD:refs/heads/replicated-branch
hyp "ForgeSync can create a branch" "$(is $GIT_RC 0)" "$GIT_OUT"

gitc -C "$WORK/r" tag -f v1.0 >/dev/null
try_git "$WORK/r" push "$URL_FS" refs/tags/v1.0
hyp "ForgeSync can push a tag" "$(is $GIT_RC 0)" "$GIT_OUT"

new_commit "$WORK/r" "forgesync with stale lease"
try_git "$WORK/r" push --force-with-lease="main:$old_sha" "$URL_FS" HEAD:refs/heads/main
hyp "A push whose expected old value is stale is rejected (safe replication)" "$( [ $GIT_RC -ne 0 ] && echo true || echo false)" "$GIT_OUT"

cur=$(branch_sha $N "$REPO" main)
try_git "$WORK/r" push --force-with-lease="main:$cur" "$URL_FS" HEAD:refs/heads/main
hyp "A push with the correct expected old value succeeds" "$(is $GIT_RC 0)" "$GIT_OUT"

gitc -C "$WORK/r" reset -q --hard HEAD~1
new_commit "$WORK/r" "forgesync force rewrite"
try_git "$WORK/r" push --force "$URL_FS" HEAD:refs/heads/main
info "ForgeSync force-push to a protected branch" "exit $GIT_RC: $GIT_OUT"

# ------------------------------------------------------------------------------------
section "Can the owner remove the protection?"
AS_TOKEN=$ALICE_TOK api DELETE $N "/repos/$REPO/branch_protections/%2A" >/dev/null
removed=$(ok_status)
hyp "Owner can't delete the replica's branch rule" "$( [ "$removed" = false ] && echo true || echo false)" \
  "HTTP $(status) $(api_msg). If the owner can remove it, protection alone isn't enough; users would need a lower role than owner/admin, or ForgeSync must detect and restore the rule."
