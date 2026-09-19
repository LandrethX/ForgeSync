-- The people a repository is shared with, as last agreed everywhere:
-- sorted "<login>:<permission>" pairs, comma-separated. A login means the
-- same person on every node, so there is nothing to map. As elsewhere, a
-- member enters the base only once every node has it and leaves only once
-- none has, so one ForgeSync couldn't write is tried again instead of its
-- absence being read as someone taking the access away.
ALTER TABLE repositories ADD COLUMN base_collaborators text NOT NULL DEFAULT '';
