#!/usr/bin/env bash
# P06 Replicating pull requests.
#
# Scenario: SE is the primary, DK a replica. Bob opens a PR on SE; ForgeSync copies the
# head branch and the PR to DK. Alice merges it on SE; ForgeSync pushes the merged base
# to DK. The DK copy must then show as merged, with the same merge commit, without DK
# creating a second merge commit.
#
# Questions:
#  - With autodetect_manual_merge off (default), does DK leave the PR open after the push?
#  - Can the merge API mark it merged with Do=manually-merged + MergeCommitID?
#  - With autodetect_manual_merge on, does DK mark it merged by itself, and as whom?
#  - Can reviews be replicated with sudo? Can ForgeSync push refs/pull/*?
. "$(dirname "$0")/lib.sh"

NAME=p06-$RUN_ID
REPO=alice/$NAME
for n in se dk; do
  for u in alice bob carol; do ensure_user $n $u; done
  create_repo $n alice "$NAME"
done
URL_SE=$(repo_url se forgesync "$(node_token se)" "$REPO")
URL_DK=$(repo_url dk forgesync "$(node_token dk)" "$REPO")

# Start both nodes from the same history: DK gets SE's main.
gitc clone -q "$URL_SE" "$WORK/r"
gitc -C "$WORK/r" push -q --force "$URL_DK" HEAD:refs/heads/main

# open_pr BRANCH TITLE  commit on BRANCH, replicate the branch, open the PR on both nodes.
# Sets PR_SE and PR_DK to the PR numbers.
open_pr() {
  gitc -C "$WORK/r" checkout -q -B "$1" origin/main
  new_commit "$WORK/r" "$2"
  gitc -C "$WORK/r" push -q "$URL_SE" "HEAD:refs/heads/$1"
  gitc -C "$WORK/r" push -q "$URL_DK" "HEAD:refs/heads/$1"
  local body; body=$(jq -nc --arg h "$1" --arg t "$2" '{head:$h, base:"main", title:$t}')
  PR_SE=$(SUDO=bob must POST se "/repos/$REPO/pulls" "$body" | jq -r .number)
  PR_DK=$(SUDO=bob must POST dk "/repos/$REPO/pulls" "$body" | jq -r .number)
}

# merge_on_se PR  merges on SE as alice and pushes the new main to DK; prints the merge commit
merge_on_se() {
  # Right after a push Forgejo may still be checking mergeability; retry briefly.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    SUDO=alice api POST se "/repos/$REPO/pulls/$1/merge" '{"Do":"merge"}' >/dev/null
    [ "$(ok_status)" = true ] && break
    sleep 2
  done
  if [ "$(ok_status)" != true ]; then echo "merge on SE failed: HTTP $(status) $(api_msg)" >&2; return 1; fi
  local sha; sha=$(api GET se "/repos/$REPO/pulls/$1" | jq -r .merge_commit_sha)
  gitc -C "$WORK/r" fetch -q origin main
  gitc -C "$WORK/r" push -q "$URL_DK" origin/main:refs/heads/main
  echo "$sha"
}

pr_state() {  # pr_state NODE PR -> "state merged merge_commit_sha merged_by"
  sleep 3   # Forgejo checks PRs against the new base in the background
  api GET "$1" "/repos/$REPO/pulls/$2" | jq -r '"\(.state) merged=\(.merged) sha=\(.merge_commit_sha // "-") by=\(.merged_by.login // "-")"'
}

# ------------------------------------------------------------------------------------
section "PR numbering"
open_pr feature-a "feature A"
hyp "The replicated PR gets the same number on both nodes (when no issue was created in between)" \
  "$(is "$PR_SE" "$PR_DK")" "SE #$PR_SE, DK #$PR_DK. PRs share the issue number sequence, so any issue on a replica shifts them."

# ------------------------------------------------------------------------------------
section "Review replicated with sudo"
rv=$(SUDO=carol api POST dk "/repos/$REPO/pulls/$PR_DK/reviews" '{"event":"APPROVED","body":"LGTM (replicated)"}')
hyp "A review replicated with Sudo: carol is carol's approval" \
  "$( [ "$(printf '%s' "$rv" | jq -r '.user.login + "/" + .state')" = carol/APPROVED ] && echo true || echo false)" \
  "HTTP $(status); $(printf '%s' "$rv" | jq -c '{user:.user.login, state, submitted_at}' 2>/dev/null)"

# ------------------------------------------------------------------------------------
section "Merge replicated, autodetect_manual_merge off (default)"
M=$(merge_on_se "$PR_SE")
st=$(pr_state dk "$PR_DK")
info "DK PR after the merged main arrives" "$st (SE merge commit $M)"

api POST dk "/repos/$REPO/pulls/$PR_DK/merge" "$(jq -nc --arg m "$M" '{Do:"manually-merged", MergeCommitID:$m}')" >/dev/null
info "Do=manually-merged with the repo's default settings" "HTTP $(status) $(api_msg)"

must PATCH dk "/repos/$REPO" '{"allow_manual_merge":true}' >/dev/null
SUDO=alice api POST dk "/repos/$REPO/pulls/$PR_DK/merge" "$(jq -nc --arg m "$M" '{Do:"manually-merged", MergeCommitID:$m}')" >/dev/null
code=$(status); msg=$(api_msg)
st=$(pr_state dk "$PR_DK")
hyp "With allow_manual_merge on, Do=manually-merged (Sudo: alice) marks the DK PR merged with SE's merge commit" \
  "$(printf '%s' "$st" | grep -q "merged=true sha=$M" && echo true || echo false)" "HTTP $code $msg; now: $st"
hyp "The manual merge is credited to the sudo user (alice), not forgesync" \
  "$(printf '%s' "$st" | grep -q "by=alice" && echo true || echo false)" "$st"
dk_main=$(branch_sha dk "$REPO" main)
hyp "No second merge commit was created on DK (main is still SE's merge commit)" "$(is "$dk_main" "$M")" "DK main=$dk_main"

api POST dk "/repos/$REPO/pulls/$PR_DK/merge" "$(jq -nc --arg m "$M" '{Do:"manually-merged", MergeCommitID:$m}')" >/dev/null
info "Repeating manually-merged on an already merged PR" "HTTP $(status) $(api_msg) (whether a retried replication is harmless)"

# ------------------------------------------------------------------------------------
section "Merge replicated, autodetect_manual_merge on"
must PATCH dk "/repos/$REPO" '{"autodetect_manual_merge":true}' >/dev/null
gitc -C "$WORK/r" fetch -q origin main
open_pr feature-b "feature B"
M=$(merge_on_se "$PR_SE")
st=$(pr_state dk "$PR_DK")
hyp "With autodetect on, DK marks the PR merged by itself once the merged main arrives" \
  "$(printf '%s' "$st" | grep -q 'merged=true' && echo true || echo false)" \
  "$st (SE merge commit $M; merged_by shows who Forgejo credits)"

# ------------------------------------------------------------------------------------
section "PR refs"
info "PR refs Forgejo maintains on DK" "$(gitc -C "$WORK/r" ls-remote "$URL_DK" | awk '{print $2}' | { grep '^refs/pull/' || true; } | tr '\n' ' ')"
try_git "$WORK/r" push "$URL_DK" HEAD:refs/pull/999/head
hyp "ForgeSync can't push refs/pull/* (Forgejo owns them; replication must exclude them)" \
  "$( [ $GIT_RC -ne 0 ] && echo true || echo false)" "exit $GIT_RC: $GIT_OUT"
