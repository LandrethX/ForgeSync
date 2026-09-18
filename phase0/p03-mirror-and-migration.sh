#!/usr/bin/env bash
# P03 Native pull mirrors as replicas, and Forgejo-to-Forgejo migration as bootstrap.
#
# Questions (mirror):
#  - Is a pull mirror read-only for everyone, including ForgeSync?
#  - Can ForgeSync trigger a sync on demand, and how quickly does it land?
#  - Can a mirror be turned into a normal repo through the API (needed for promotion)?
#  - Can issues be used on a mirror?
# Questions (migration with metadata, source SE -> target DK):
#  - Are issue numbers, including gaps, preserved?
#  - Are authors and creation times preserved, or only recorded as "original author"?
. "$(dirname "$0")/lib.sh"

NAME=p03-$RUN_ID
SRC=alice/$NAME
for n in se dk; do ensure_user $n alice; done
for u in bob carol; do ensure_user se $u; done
create_repo se alice "$NAME"

SE_TOK=$(node_token se)
DK_TOK=$(node_token dk)

# Some history to migrate: issues #1..#3 by different users, then delete #2 to leave a gap.
SUDO=bob   must POST se "/repos/$SRC/issues" '{"title":"first issue, by bob"}' >/dev/null
SUDO=alice must POST se "/repos/$SRC/issues" '{"title":"second issue, deleted later"}' >/dev/null
SUDO=alice must POST se "/repos/$SRC/issues" '{"title":"third issue, by alice"}' >/dev/null
SUDO=carol must POST se "/repos/$SRC/issues/1/comments" '{"body":"comment by carol"}' >/dev/null
must POST se "/repos/$SRC/labels" '{"name":"bug","color":"#ee0701"}' >/dev/null
must POST se "/repos/$SRC/milestones" '{"title":"v1"}' >/dev/null
api DELETE se "/repos/$SRC/issues/2" >/dev/null
info "Deleting issue #2 on the source to leave a numbering gap" "HTTP $(status)"

# ------------------------------------------------------------------------------------
section "Pull mirror SE -> DK"
MIRROR=alice/$NAME-mirror
api POST dk /repos/migrate "$(jq -nc --arg c "$(node_url se)/$SRC.git" --arg n "$NAME-mirror" \
  '{clone_addr:$c, repo_owner:"alice", repo_name:$n, mirror:true, service:"git", mirror_interval:"8h"}')" >/dev/null
hyp "ForgeSync can create a pull mirror on DK of a repo on SE" "$(ok_status)" "HTTP $(status) $(api_msg)"

if [ "$(ok_status)" = true ]; then
  gitc clone -q "$(repo_url dk forgesync "$DK_TOK" "$MIRROR")" "$WORK/m"
  new_commit "$WORK/m" "push into mirror"
  try_git "$WORK/m" push "$(repo_url dk alice "$(user_token dk alice)" "$MIRROR")" HEAD:refs/heads/main
  hyp "Owner can't push to the mirror" "$( [ $GIT_RC -ne 0 ] && echo true || echo false)" "$GIT_OUT"
  try_git "$WORK/m" push "$(repo_url dk forgesync "$DK_TOK" "$MIRROR")" HEAD:refs/heads/main
  info "ForgeSync push to the mirror" "exit $GIT_RC: $GIT_OUT (rejected means ForgeSync can only update it via mirror-sync)"

  # New commit on the source, then ask DK to sync now.
  gitc clone -q "$(repo_url se forgesync "$SE_TOK" "$SRC")" "$WORK/s"
  new_commit "$WORK/s" "new work on SE"
  gitc -C "$WORK/s" push -q origin HEAD:main
  want=$(gitc -C "$WORK/s" rev-parse HEAD)
  t0=$(date +%s)
  api POST dk "/repos/$MIRROR/mirror-sync" >/dev/null
  sync_status=$(status)
  got=""
  for i in $(seq 1 30); do
    got=$(branch_sha dk "$MIRROR" main)
    [ "$got" = "$want" ] && break
    sleep 1
  done
  hyp "mirror-sync pulls a new source commit on demand" "$(is "$got" "$want")" \
    "mirror-sync HTTP $sync_status; landed after about $(( $(date +%s) - t0 ))s"

  AS_TOKEN=$(user_token dk alice) api POST dk "/repos/$MIRROR/issues" '{"title":"issue on a mirror"}' >/dev/null
  info "Creating an issue on a mirror" "HTTP $(status) $(api_msg)"

  api PATCH dk "/repos/$MIRROR" '{"mirror":false}' >/dev/null
  still=$(api GET dk "/repos/$MIRROR" | jq -r .mirror)
  hyp "The API can't convert a mirror into a normal repository (promotion needs another route)" "$(is "$still" true)" \
    "PATCH {mirror:false} -> HTTP $(status); mirror is now: $still"
fi

# ------------------------------------------------------------------------------------
section "Migration with metadata SE -> DK (service=gitea)"
# Let the clock move on, so "creation times preserved" can't pass just because
# the issues and the migration happened in the same second.
sleep 3
IMPORT=alice/$NAME-import
api POST dk /repos/migrate "$(jq -nc --arg c "$(node_url se)/$SRC" --arg n "$NAME-import" --arg t "$SE_TOK" \
  '{clone_addr:$c, repo_owner:"alice", repo_name:$n, service:"gitea", auth_token:$t, mirror:false,
    issues:true, labels:true, milestones:true, releases:true, pull_requests:true, wiki:true}')" >/dev/null
hyp "Forgejo can migrate a repo with metadata from another Forgejo" "$(ok_status)" "HTTP $(status) $(api_msg)"

if [ "$(ok_status)" = true ]; then
  src_issues=$(api GET se "/repos/$SRC/issues?state=all&type=issues&limit=50" | jq -c 'sort_by(.number)')
  dst_issues=$(api GET dk "/repos/$IMPORT/issues?state=all&type=issues&limit=50" | jq -c 'sort_by(.number)')
  src_nums=$(printf '%s' "$src_issues" | jq -r '[.[].number] | join(",")')
  dst_nums=$(printf '%s' "$dst_issues" | jq -r '[.[].number] | join(",")')
  hyp "Issue numbers are preserved, including the gap" "$(is "$dst_nums" "$src_nums")" "source #$src_nums, migrated #$dst_nums"

  src_times=$(printf '%s' "$src_issues" | jq -r '[.[].created_at] | join(",")')
  dst_times=$(printf '%s' "$dst_issues" | jq -r '[.[].created_at] | join(",")')
  hyp "Issue creation times are preserved" "$(is "$dst_times" "$src_times")" "source $src_times | migrated $dst_times"

  info "Authors after migration (source login -> migrated user / original_author)" \
    "$(jq -nr --argjson s "$src_issues" --argjson d "$dst_issues" \
      '[range(0; $s|length) as $i | "#\($s[$i].number) \($s[$i].user.login) -> \($d[$i].user.login // "?") / \($d[$i].original_author // "")"] | join("; ")')"

  c=$(api GET dk "/repos/$IMPORT/issues/1/comments" | jq -c '[.[] | {user:.user.login, original_author, created_at}]')
  info "Comments after migration" "$c"
  info "Labels / milestones after migration" \
    "$(api GET dk "/repos/$IMPORT/labels" | jq length) labels, $(api GET dk "/repos/$IMPORT/milestones?state=all" | jq length) milestones"
fi
