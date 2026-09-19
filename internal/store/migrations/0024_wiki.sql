-- What ForgeSync last wrote to each node's wiki. The wiki is a second git
-- repository, so this is replicated_refs again, kept apart because the two
-- are replicated separately and each replaces its own set.
CREATE TABLE wiki_refs (
    repository_id uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    node          text NOT NULL REFERENCES nodes (name),
    ref           text NOT NULL,
    sha           text NOT NULL,
    PRIMARY KEY (repository_id, node, ref)
);
