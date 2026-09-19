-- An issue's and a comment's reactions, as last agreed everywhere: sorted
-- "<login>:<content>" pairs, comma-separated. A reaction belongs to a
-- person, and a login means the same person on every node, so there's
-- nothing to map. An element only enters the base once every node has it,
-- and only leaves once no node has it, so a copy ForgeSync couldn't write
-- is retried instead of being read as someone's change.
ALTER TABLE issues ADD COLUMN base_reactions text NOT NULL DEFAULT '';
ALTER TABLE issue_comments ADD COLUMN base_reactions text NOT NULL DEFAULT '';
