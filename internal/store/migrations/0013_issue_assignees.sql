-- An issue's assignees (sorted logins, comma-separated), as last agreed
-- everywhere. Logins are the same on every node: SceneID's sub is the
-- global user key, so no per-node mapping is needed.
ALTER TABLE issues ADD COLUMN base_assignees text NOT NULL DEFAULT '';
