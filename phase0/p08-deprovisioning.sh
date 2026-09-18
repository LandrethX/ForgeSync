#!/usr/bin/env bash
# P08 Deprovisioning: a user is disabled in SceneID.
#
# SceneID only guards new logins. Forgejo API tokens, SSH keys and open web sessions
# don't go through SceneID, so they probably keep working until ForgeSync acts on every
# node.
#
# Questions:
#  - What still works on the Forgejo nodes after the user is disabled in SceneID?
#  - Can ForgeSync find disabled users by polling SceneID with its own client?
#  - Does the admin API's prohibit_login cut off tokens, SSH and sessions on each node?
#  - Is it reversible when the user is re-enabled?
. "$(dirname "$0")/lib.sh"

U=p08u$RUN_ID
REPO=$U/private-$RUN_ID
kc_create_user "$U" >/dev/null
for n in se dk; do
  final=$(sceneid_login $n "$U" "$U-pw" "$WORK/jar-$n")
done
create_repo dk "$U" "private-$RUN_ID"
must PATCH dk "/repos/$REPO" '{"private":true}' >/dev/null
KEY=$(new_key "$U")
must POST dk "/admin/users/$U/keys" "$(jq -nc --arg k "$(cat "$KEY.pub")" '{title:"laptop", key:$k}')" >/dev/null
TOK_SE=$(user_token se "$U")
TOK_DK=$(user_token dk "$U")

# access_report LABEL  records what the user can still do (token SE/DK, SSH DK, sessions)
# and sets ANY_ACCESS/ALL_ACCESS
access_report() {
  local t_se t_dk s_se s_dk ssh
  t_se=$(AS_TOKEN=$TOK_SE api GET se /user | jq -r '.login // empty' 2>/dev/null || true)
  t_dk=$(AS_TOKEN=$TOK_DK api GET dk /user | jq -r '.login // empty' 2>/dev/null || true)
  s_se=$(session_login "$WORK/jar-se" se)
  s_dk=$(session_login "$WORK/jar-dk" dk)
  try_ssh_git "$KEY" "$WORK" ls-remote "$(ssh_url dk "$REPO")"
  ssh=$([ $GIT_RC -eq 0 ] && echo "$U" || echo "")
  ACCESS="token SE:${t_se:-no} | token DK:${t_dk:-no} | session SE:${s_se:-no} | session DK:${s_dk:-no} | SSH DK:${ssh:-no}"
  ANY_ACCESS=false; ALL_ACCESS=true
  for v in "$t_se" "$t_dk" "$s_se" "$s_dk" "$ssh"; do
    if [ "$v" = "$U" ]; then ANY_ACCESS=true; else ALL_ACCESS=false; fi
  done
  info "Access $1" "$ACCESS"
}

# ------------------------------------------------------------------------------------
section "Baseline"
access_report "before any change"
hyp "Before deprovisioning, token, session and SSH access all work (sanity check)" "$ALL_ACCESS" "$ACCESS"

# ------------------------------------------------------------------------------------
section "Disabled in SceneID only"
code=$(kc_user_update "$U" '{"enabled":false}')
hyp "ForgeSync's SceneID client can disable a user" "$(is "$code" 204)" "HTTP $code"

final=$(sceneid_login dk "$U" "$U-pw" "$WORK/jar-new") || final="(login flow failed)"
hyp "A new SceneID login is refused" "$( [ -z "$(session_login "$WORK/jar-new" dk)" ] && echo true || echo false)" "ended at $final"

access_report "after disabling in SceneID"
hyp "Existing tokens, sessions and SSH keys keep working after SceneID disables the user (the gap ForgeSync must close)" \
  "$ANY_ACCESS" "$ACCESS"

found=$(curl -fsS -H "Authorization: Bearer $(kc_admin_token)" "$SCENEID/admin/realms/sceneid/users?enabled=false&max=1000" \
  | jq -r --arg u "$U" '[.[] | select(.username == $u)] | length')
hyp "ForgeSync can find disabled users by polling SceneID (users?enabled=false)" "$(is "$found" 1)"

# ------------------------------------------------------------------------------------
section "ForgeSync sets prohibit_login on every node"
for n in se dk; do
  api PATCH $n "/admin/users/$U" '{"prohibit_login":true}' >/dev/null
  hyp "Admin API sets prohibit_login on $n" "$(ok_status)" "HTTP $(status) $(api_msg)"
done
access_report "after prohibit_login"
hyp "prohibit_login cuts off tokens, sessions and SSH on both nodes" "$( [ "$ANY_ACCESS" = false ] && echo true || echo false)" "$ACCESS"

# ------------------------------------------------------------------------------------
section "Re-enabled"
kc_user_update "$U" '{"enabled":true}' >/dev/null
for n in se dk; do api PATCH $n "/admin/users/$U" '{"prohibit_login":false}' >/dev/null; done
for n in se dk; do sceneid_login $n "$U" "$U-pw" "$WORK/jar-$n" >/dev/null || true; done
access_report "after re-enabling"
hyp "Re-enabling in SceneID and clearing prohibit_login restores access, with the same tokens and keys" "$ALL_ACCESS" "$ACCESS"
