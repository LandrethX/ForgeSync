-- Labels and milestones: repository-level items issues refer to. Like
-- issues, each has one identity with a copy per node (Forgejo ids differ);
-- base is the last value of each field that agreed everywhere.
CREATE TABLE repo_items (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    kind          text NOT NULL CHECK (kind IN ('label', 'milestone')),
    origin_node   text NOT NULL REFERENCES nodes (name),
    base          jsonb NOT NULL,
    -- Deleted on the primary; changed copies elsewhere are kept as conflicts
    -- and never copied back.
    deleted_at    timestamptz
);
CREATE INDEX repo_items_repository ON repo_items (repository_id);

CREATE TABLE repo_item_copies (
    item_id    uuid NOT NULL REFERENCES repo_items (id) ON DELETE CASCADE,
    kind       text NOT NULL, -- labels and milestones have separate id sequences
    node       text NOT NULL REFERENCES nodes (name),
    forgejo_id bigint NOT NULL,
    PRIMARY KEY (item_id, node),
    UNIQUE (kind, node, forgejo_id)
);

-- An issue's labels (sorted item ids, comma-separated) and milestone (item
-- id or ''), as last agreed everywhere.
ALTER TABLE issues ADD COLUMN base_labels text NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN base_milestone text NOT NULL DEFAULT '';
