#!/usr/bin/env bash
# P01 Identity: SceneID-only users, and pre-creating users on nodes they haven't visited.
#
# Questions:
#  - Does Forgejo store the SceneID subject (sub) somewhere ForgeSync can read and search?
#  - If ForgeSync creates a user in advance, does their first SceneID login attach to that
#    account instead of creating a duplicate? With source_id+login_name, and without?
#  - Is local sign-up blocked, while local admins can still use password auth?
. "$(dirname "$0")/lib.sh"

SRC_SE=$(sceneid_source_id se)
SRC_DK=$(sceneid_source_id dk)
info "SceneID login source id" "se=$SRC_SE dk=$SRC_DK (ids are per node; ForgeSync must map them)"

# Fresh SceneID users each run, so no node has seen them before.
U_AUTO=p01a$RUN_ID   # never pre-created: plain auto-registration
U_SRC=p01b$RUN_ID    # pre-created with source_id + login_name=sub
U_LOCAL=p01c$RUN_ID  # pre-created as a plain local user
U_RENAME=p01d$RUN_ID # pre-created with the right sub but a different username

# ------------------------------------------------------------------------------------
section "Automatic sign-up on first SceneID login"
SUB_AUTO=$(kc_create_user "$U_AUTO")
final=$(sceneid_login se "$U_AUTO" "$U_AUTO-pw" "$WORK/jar-a")
who=$(session_login "$WORK/jar-a" se)
hyp "First SceneID login creates the account automatically" "$(is "$who" "$U_AUTO")" "landed on $final, session user '$who'"

admin_rec=$(admin_users se | jq -c --arg u "$U_AUTO" '.[] | select(.login == $u) | {id, login, source_id, login_name}')
info "How Forgejo records an auto-registered SceneID user" "$admin_rec (SceneID sub = $SUB_AUTO)"
hyp "login_name holds the SceneID sub" "$(is "$(printf '%s' "$admin_rec" | jq -r .login_name)" "$SUB_AUTO")"

found=$(api GET se "/admin/users?source_id=$SRC_SE&login_name=$SUB_AUTO" | jq -r '[.[].login] | join(",")')
hyp "Admin API can look up a user by SceneID sub (source_id + login_name filter)" "$(is "$found" "$U_AUTO")" "result: [$found]"

# ------------------------------------------------------------------------------------
section "Pre-created with source_id + login_name, then first login (DK)"
SUB_SRC=$(kc_create_user "$U_SRC")
pre=$(api POST dk /admin/users "$(jq -nc --arg u "$U_SRC" --arg s "$SUB_SRC" --argjson src "$SRC_DK" \
  '{username:$u, email:($u+"@sceneid.test"), source_id:$src, login_name:$s, must_change_password:false}')")
hyp "Admin API can create a user bound to the SceneID source (no password)" "$(ok_status)" \
  "HTTP $(status) $(api_msg) $(printf '%s' "$pre" | jq -c '{id, login, source_id, login_name}' 2>/dev/null)"
if [ "$(ok_status)" = true ]; then
pre_id=$(printf '%s' "$pre" | jq -r .id)
final=$(sceneid_login dk "$U_SRC" "$U_SRC-pw" "$WORK/jar-b")
who=$(session_login "$WORK/jar-b" dk)
after_id=""; [ -n "$who" ] && after_id=$(api GET dk "/users/$who" | jq -r '.id // empty')
dups=$(admin_users dk | jq -r --arg u "$U_SRC" '[.[] | select(.login | startswith($u))] | length')
attached=false
if [ "$after_id" = "$pre_id" ] && [ "$dups" = 1 ] && ! printf '%s' "$final" | grep -q link_account; then attached=true; fi
hyp "First login attaches to the pre-created account (same id, no link page, no duplicate)" "$attached" \
  "landed on $final; session user '$who' id=$after_id (pre-created id=$pre_id); accounts with this name: $dups"
fi

# ------------------------------------------------------------------------------------
section "Pre-created as a plain local user, then first login (DK)"
kc_create_user "$U_LOCAL" >/dev/null
pre=$(must POST dk /admin/users "$(jq -nc --arg u "$U_LOCAL" --arg p "$(openssl rand -hex 16)" \
  '{username:$u, email:($u+"@sceneid.test"), password:$p, must_change_password:false}')")
pre_id=$(printf '%s' "$pre" | jq -r .id)
final=$(sceneid_login dk "$U_LOCAL" "$U_LOCAL-pw" "$WORK/jar-c")
who=$(session_login "$WORK/jar-c" dk)
after=$(admin_users dk | jq -c --arg u "$U_LOCAL" '[.[] | select(.login | startswith($u)) | {id, login, source_id, login_name}]')
hyp "ACCOUNT_LINKING=auto links SceneID to a pre-created local account" \
  "$( [ -n "$who" ] && [ "$(api GET dk "/users/$who" | jq -r '.id // empty')" = "$pre_id" ] && echo true || echo false)" \
  "landed on $final; session user '$who'; accounts now: $after"
info "After linking, the account's source_id/login_name" "$after (source_id 0 means it is still a local account and keeps its local password)"

# ------------------------------------------------------------------------------------
section "Pre-created with the right sub but a different username (DK)"
SUB_RENAME=$(kc_create_user "$U_RENAME")
api POST dk /admin/users "$(jq -nc --arg u "${U_RENAME}x" --arg e "$U_RENAME@sceneid.test" --arg s "$SUB_RENAME" \
  --argjson src "$SRC_DK" '{username:$u, email:$e, source_id:$src, login_name:$s, must_change_password:false}')" >/dev/null
if [ "$(ok_status)" != true ]; then
  info "Skipped: could not pre-create the user" "HTTP $(status) $(api_msg)"
else
final=$(sceneid_login dk "$U_RENAME" "$U_RENAME-pw" "$WORK/jar-d")
who=$(session_login "$WORK/jar-d" dk)
info "Login when the username differs but sub and email match" \
  "landed on $final; session user '$who' (pre-created as '${U_RENAME}x'). Shows whether Forgejo matches on sub, on email or on username."
fi

# ------------------------------------------------------------------------------------
section "Local accounts"
page=$(curl -sS "$(node_url se)/user/sign_up")
hyp "The sign-up page offers no local password registration" \
  "$(printf '%s' "$page" | grep -q 'name="password"' && echo false || echo true)"

code=$(curl -sS -o /dev/null -w '%{http_code}' -u "siteadmin:$FORGEJO_SITEADMIN_PASSWORD" "$(node_url se)/api/v1/user")
hyp "Local admin 'siteadmin' can still authenticate with a password" "$(is "$code" 200)" "HTTP $code"

for n in se dk; do
  locals=$(admin_users "$n" | jq -r '[.[] | select(.source_id == 0) | .login + (if .is_admin then "(admin)" else "" end)] | join(", ")')
  info "Local-source accounts on $n (what a ForgeSync compliance check would flag, apart from admins)" "$locals"
done
