#!/usr/bin/env bash
# P07 SSH keys for SceneID users on every node.
#
# SceneID users have no Git password, so SSH keys (or per-node tokens) are how they use
# Git. For "use any node", keys must exist on every node. Two routes:
#  A) ForgeSync copies keys with the admin API.
#  B) SceneID sends keys in a login claim and Forgejo syncs them itself at each login
#     (auth source option --attribute-ssh-public-key; the claim must be a JSON array).
#
# Questions:
#  - Can ForgeSync read a user's keys, and add them on another node, even before the
#    user has ever logged in there? Does Git over SSH then work?
#  - Does deleting a key through the admin API cut off access?
#  - Can the same key belong to two users on one node?
#  - Route B: are keys synced at login, removed when they leave SceneID, and are keys
#    ForgeSync added (route A) left alone?
. "$(dirname "$0")/lib.sh"

NAME=p07-$RUN_ID
REPO=alice/$NAME
for n in se dk; do ensure_user $n alice; ensure_user $n bob; done
create_repo dk alice "$NAME"

# ------------------------------------------------------------------------------------
section "Route A: ForgeSync copies keys with the admin API"
KEY=$(new_key "alice-$RUN_ID")
AS_TOKEN=$(user_token se alice) must POST se /user/keys \
  "$(jq -nc --arg k "$(cat "$KEY.pub")" --arg t "laptop-$RUN_ID" '{title:$t, key:$k}')" >/dev/null
keys=$(api GET se /users/alice/keys | jq -c --arg t "laptop-$RUN_ID" '[.[] | select(.title == $t) | {id, title, fingerprint}]')
hyp "ForgeSync can read a key alice added herself on SE" "$( [ "$(printf '%s' "$keys" | jq length)" = 1 ] && echo true || echo false)" "$keys"

added=$(api POST dk /admin/users/alice/keys "$(jq -nc --arg k "$(cat "$KEY.pub")" --arg t "laptop-$RUN_ID" '{title:$t, key:$k}')")
hyp "ForgeSync can add the same key for alice on DK through the admin API" "$(ok_status)" "HTTP $(status) $(api_msg)"
DK_KEY_ID=$(printf '%s' "$added" | jq -r '.id // empty')

try_ssh_git "$KEY" "$WORK" clone -q "$(ssh_url dk "$REPO")" "$WORK/c"
hyp "Alice can clone over SSH on DK with the copied key" "$(is $GIT_RC 0)" "$GIT_OUT"
if [ $GIT_RC -eq 0 ]; then
  new_commit "$WORK/c" "pushed over ssh"
  try_ssh_git "$KEY" "$WORK/c" push -q origin HEAD:main
  hyp "Alice can push over SSH on DK with the copied key" "$(is $GIT_RC 0)" "$GIT_OUT"
fi

api POST dk /admin/users/bob/keys "$(jq -nc --arg k "$(cat "$KEY.pub")" '{title:"same key", key:$k}')" >/dev/null
hyp "The same key can't be added to a second user on the same node" \
  "$( [ "$(ok_status)" = false ] && echo true || echo false)" "HTTP $(status) $(api_msg). ForgeSync must treat this as a conflict, not retry."

# Pre-created user who has never logged in on DK.
U=p07u$RUN_ID
SUB=$(kc_create_user "$U")
api POST dk /admin/users "$(jq -nc --arg u "$U" --arg s "$SUB" --argjson src "$(sceneid_source_id dk)" \
  '{username:$u, email:($u+"@sceneid.test"), source_id:$src, login_name:$s, must_change_password:false}')" >/dev/null
if [ "$(ok_status)" = true ]; then
  UKEY=$(new_key "$U")
  api POST dk "/admin/users/$U/keys" "$(jq -nc --arg k "$(cat "$UKEY.pub")" '{title:"pre-provisioned", key:$k}')" >/dev/null
  hyp "Keys can be added for a pre-created user who has never logged in" "$(ok_status)" "HTTP $(status) $(api_msg)"
else
  info "Skipped the pre-created user check: pre-creation was rejected" "HTTP $(status) $(api_msg) (see p01)"
fi

if [ -n "$DK_KEY_ID" ]; then
  api DELETE dk "/admin/users/alice/keys/$DK_KEY_ID" >/dev/null
  del=$(status)
  sleep 1
  try_ssh_git "$KEY" "$WORK" ls-remote "$(ssh_url dk "$REPO")"
  hyp "After the admin API deletes the key, SSH access on DK stops" "$( [ $GIT_RC -ne 0 ] && echo true || echo false)" \
    "DELETE HTTP $del; ls-remote exit $GIT_RC: $GIT_OUT"
fi

# ------------------------------------------------------------------------------------
section "Route B: keys from a SceneID login claim (DK)"
KC=$(kc_master_token)
# Allow custom user attributes in the realm, and send the 'sshkeys' attribute as a
# multivalued 'ssh_public_keys' claim to Forgejo.
profile=$(curl -fsS -H "Authorization: Bearer $KC" "$SCENEID/admin/realms/sceneid/users/profile")
curl -fsS -o /dev/null -X PUT -H "Authorization: Bearer $KC" -H 'Content-Type: application/json' \
  --data "$(printf '%s' "$profile" | jq -c '.unmanagedAttributePolicy = "ENABLED"')" \
  "$SCENEID/admin/realms/sceneid/users/profile"
CID=$(curl -fsS -H "Authorization: Bearer $KC" "$SCENEID/admin/realms/sceneid/clients?clientId=forgejo" | jq -r '.[0].id')
if ! curl -fsS -H "Authorization: Bearer $KC" "$SCENEID/admin/realms/sceneid/clients/$CID/protocol-mappers/models" \
     | jq -e '.[] | select(.name == "ssh_public_keys")' >/dev/null; then
  curl -fsS -o /dev/null -X POST -H "Authorization: Bearer $KC" -H 'Content-Type: application/json' \
    "$SCENEID/admin/realms/sceneid/clients/$CID/protocol-mappers/models" \
    --data '{"name":"ssh_public_keys","protocol":"openid-connect","protocolMapper":"oidc-usermodel-attribute-mapper",
             "config":{"user.attribute":"sshkeys","claim.name":"ssh_public_keys","multivalued":"true",
                       "jsonType.label":"String","id.token.claim":"true","access.token.claim":"true","userinfo.token.claim":"true"}}'
fi
SRC_DK=$(sceneid_source_id dk)
fj dk admin auth update-oauth --id "$SRC_DK" --attribute-ssh-public-key ssh_public_keys >/dev/null
# Put the default back even if a check below aborts the probe.
trap 'fj dk admin auth update-oauth --id "$SRC_DK" --attribute-ssh-public-key "" >/dev/null || true; rm -rf "$WORK"' EXIT
info "Configured SceneID claim 'ssh_public_keys' and set it as the DK login source's SSH key attribute" "source id $SRC_DK"

V=p07v$RUN_ID
kc_create_user "$V" >/dev/null
CLAIM_KEY=$(new_key "$V-claim")
MANUAL_KEY=$(new_key "$V-manual")
code=$(kc_user_update "$V" "$(jq -nc --arg k "$(cat "$CLAIM_KEY.pub")" '{attributes:{sshkeys:[$k]}}')")
info "Stored a key in the SceneID user's 'sshkeys' attribute" "HTTP $code"

final=$(sceneid_login dk "$V" "$V-pw" "$WORK/jar-v")
keys=$(api GET dk "/users/$V/keys" | jq -c '[.[] | .key]')
hyp "First login creates the user with the key from the claim" \
  "$(printf '%s' "$keys" | grep -qF "$(cut -d' ' -f2 "$CLAIM_KEY.pub")" && echo true || echo false)" \
  "landed on $final; keys on DK: $(printf '%s' "$keys" | jq length)"

api POST dk "/admin/users/$V/keys" "$(jq -nc --arg k "$(cat "$MANUAL_KEY.pub")" '{title:"added by forgesync", key:$k}')" >/dev/null
code=$(kc_user_update "$V" '{"attributes":{"sshkeys":[]}}')
final=$(sceneid_login dk "$V" "$V-pw" "$WORK/jar-v2")
keys=$(api GET dk "/users/$V/keys" | jq -c '[.[] | .key]')
hyp "After the key is removed in SceneID, the next login removes it on DK" \
  "$(printf '%s' "$keys" | grep -qF "$(cut -d' ' -f2 "$CLAIM_KEY.pub")" && echo false || echo true)" \
  "SceneID update HTTP $code; landed on $final"
hyp "A key ForgeSync added through the admin API survives that login sync" \
  "$(printf '%s' "$keys" | grep -qF "$(cut -d' ' -f2 "$MANUAL_KEY.pub")" && echo true || echo false)" \
  "keys now on DK: $(printf '%s' "$keys" | jq length)"

info "The DK login source's SSH key attribute is reset when this probe exits" "so other probes see the default configuration"
