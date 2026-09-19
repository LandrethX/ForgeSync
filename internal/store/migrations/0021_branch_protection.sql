-- The branch protection rules the nodes last agreed on, as
-- "<rule>:<digest of its settings>" members. ForgeSync's own guard on the
-- replicas (the ** rule) is not among them: it belongs to ForgeSync, not
-- to the owner, and never travels to the primary.
ALTER TABLE repositories ADD COLUMN base_protection text NOT NULL DEFAULT '';
