-- A repository's Actions variables as the nodes last agreed them:
-- "<name>:<digest of its value>" members. Secrets aren't here and never
-- will be: Forgejo gives back a secret's name but never its value, so
-- ForgeSync can say which node is missing one but can't copy it.
ALTER TABLE repositories ADD COLUMN base_action_variables text NOT NULL DEFAULT '';
