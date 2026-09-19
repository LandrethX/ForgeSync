-- A repository's settings as the nodes last agreed them: the single-valued
-- ones as a map, and its topics as sorted members. The default branch is
-- not among them -- replication already follows the primary's, and a
-- difference is the detector's default_branch_mismatch -- and neither is
-- archived, which would stop replication to a node that took it.
ALTER TABLE repositories ADD COLUMN base_metadata jsonb NOT NULL DEFAULT '{}';
ALTER TABLE repositories ADD COLUMN base_topics text NOT NULL DEFAULT '';
