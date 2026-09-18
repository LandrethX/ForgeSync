-- Repository inventory: what exists on which node, as found by the periodic
-- scan. This is observation only; replication state comes later.

-- One row per repository across all nodes. Until replication assigns
-- identities, repositories are matched across nodes by owner/name
-- (case-insensitively, as Forgejo does).
CREATE TABLE repositories (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    full_name     text NOT NULL,
    primary_node  text REFERENCES nodes (name),
    first_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX repositories_full_name ON repositories (lower(full_name));

-- What the last successful scan of a node saw for a repository.
CREATE TABLE repository_replicas (
    repository_id      uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    node               text NOT NULL REFERENCES nodes (name),
    present            boolean NOT NULL,
    forgejo_id         bigint NOT NULL DEFAULT 0,
    private            boolean NOT NULL DEFAULT false,
    fork               boolean NOT NULL DEFAULT false,
    mirror             boolean NOT NULL DEFAULT false,
    archived           boolean NOT NULL DEFAULT false,
    empty              boolean NOT NULL DEFAULT false,
    default_branch     text NOT NULL DEFAULT '',
    head_sha           text NOT NULL DEFAULT '',
    head_error         text NOT NULL DEFAULT '',
    forgejo_updated_at timestamptz,
    last_seen_at       timestamptz,          -- last scan that found it
    checked_at         timestamptz NOT NULL, -- last successful scan of the node
    PRIMARY KEY (repository_id, node)
);
CREATE INDEX repository_replicas_node ON repository_replicas (node);

-- Outcome of the latest scan of each node.
CREATE TABLE inventory_scans (
    node            text PRIMARY KEY REFERENCES nodes (name),
    started_at      timestamptz NOT NULL,
    finished_at     timestamptz NOT NULL,
    ok              boolean NOT NULL,
    error           text NOT NULL DEFAULT '',
    repositories    integer NOT NULL DEFAULT 0,
    last_success_at timestamptz
);
