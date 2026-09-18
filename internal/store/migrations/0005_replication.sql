-- Git replication from a repository's primary to its replicas.

-- The value ForgeSync last wrote to (or confirmed on) each replica, per ref.
-- It's the base of the three-way compare: it tells "deleted on the primary"
-- from "created on the replica", and "replica unchanged" from "changed".
CREATE TABLE replicated_refs (
    repository_id uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    node          text NOT NULL REFERENCES nodes (name),
    ref           text NOT NULL,
    sha           text NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (repository_id, node, ref)
);

-- Latest replication outcome per repository and replica.
CREATE TABLE replica_sync (
    repository_id     uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    node              text NOT NULL REFERENCES nodes (name),
    state             text NOT NULL, -- synced, conflict, error, waiting, missing
    detail            text NOT NULL DEFAULT '',
    last_attempt_at   timestamptz NOT NULL,
    last_success_at   timestamptz,
    out_of_sync_since timestamptz,
    refs_updated      integer NOT NULL DEFAULT 0, -- in the latest attempt
    PRIMARY KEY (repository_id, node)
);
CREATE INDEX replica_sync_state ON replica_sync (state);
