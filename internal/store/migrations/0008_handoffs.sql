-- Diverged branches handed to the repository's owner as a pull request on
-- the primary. The owner decides there: merging keeps the replica's commits
-- (the replica then just fast-forwards); closing keeps the primary's version
-- (ForgeSync resets the replica, and the pull request's branch stays as a
-- backup until backup_until).
CREATE TABLE conflict_handoffs (
    id            bigserial PRIMARY KEY,
    repository_id uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    ref           text NOT NULL,   -- the diverged branch, refs/heads/...
    sha           text NOT NULL,   -- the replicas' head that was handed off
    primary_node  text NOT NULL REFERENCES nodes (name),
    nodes         text[] NOT NULL, -- replicas at sha when handed off
    branch        text NOT NULL,   -- the pull request's branch on the primary, refs/heads/forgesync/conflict/...
    pr_number     bigint NOT NULL,
    pr_url        text NOT NULL DEFAULT '',
    -- open: waiting for the owner. merged: they kept the replica's commits.
    -- kept_primary: closed without merging; replicas reset, backup kept.
    -- expired: backup branch deleted. gone: the pull request disappeared.
    state         text NOT NULL CHECK (state IN ('open', 'merged', 'kept_primary', 'expired', 'gone')),
    opened_at     timestamptz NOT NULL,
    decided_at    timestamptz,
    backup_until  timestamptz
);
-- One open hand-off per diverged head.
CREATE UNIQUE INDEX conflict_handoffs_open ON conflict_handoffs (repository_id, ref, sha) WHERE state = 'open';
CREATE INDEX conflict_handoffs_active ON conflict_handoffs (repository_id) WHERE state IN ('open', 'kept_primary');
