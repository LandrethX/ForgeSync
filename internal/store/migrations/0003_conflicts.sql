-- Conflicts between nodes. A conflict is opened when detected and cleared
-- automatically once a later check no longer finds it. ForgeSync never
-- resolves a conflict by changing data; people fix it (e.g. in Git) and the
-- next check clears it. Operators can acknowledge a conflict with a note.
CREATE TABLE conflicts (
    id              bigserial PRIMARY KEY,
    repository_id   uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    kind            text NOT NULL, -- git_diverged, default_branch_mismatch
    ref             text NOT NULL DEFAULT '',
    state           text NOT NULL, -- open, cleared
    details         jsonb NOT NULL DEFAULT '{}',
    detected_at     timestamptz NOT NULL,
    last_seen_at    timestamptz NOT NULL,
    cleared_at      timestamptz,
    acknowledged_by text NOT NULL DEFAULT '',
    acknowledged_at timestamptz,
    note            text NOT NULL DEFAULT ''
);
-- At most one open conflict per repository, kind and ref.
CREATE UNIQUE INDEX conflicts_open ON conflicts (repository_id, kind, ref) WHERE state = 'open';
CREATE INDEX conflicts_state ON conflicts (state, detected_at);
