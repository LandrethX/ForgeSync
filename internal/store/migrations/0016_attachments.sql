-- An issue's and a comment's attachments, as last agreed everywhere:
-- sorted "<size>:<name>" pairs, comma-separated. Forgejo gives each copy
-- its own id and uuid, and nothing in one says two are the same file, so
-- size and name are what matches them. As with reactions, a file enters
-- the base only once every node has it and leaves only once none has.
ALTER TABLE issues ADD COLUMN base_attachments text NOT NULL DEFAULT '';
ALTER TABLE issue_comments ADD COLUMN base_attachments text NOT NULL DEFAULT '';
