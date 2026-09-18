-- Issue replication. Each issue and comment has one identity across nodes
-- and a copy on each node (its number and Forgejo id there differ as they
-- must: Forgejo assigns them). base_* is the last value ForgeSync saw agree
-- everywhere: the base of a three-way merge per field.
CREATE TABLE issues (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    origin_node   text NOT NULL REFERENCES nodes (name),
    author        text NOT NULL,
    created_at    timestamptz NOT NULL, -- on the origin node
    base_title    text NOT NULL,
    base_body     text NOT NULL,
    base_state    text NOT NULL,
    -- Deleted on the primary; copies changed elsewhere since are kept (as
    -- conflicts) but never copied back to the primary.
    deleted_at    timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX issues_repository ON issues (repository_id);

CREATE TABLE issue_copies (
    issue_id   uuid NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    node       text NOT NULL REFERENCES nodes (name),
    number     bigint NOT NULL,
    forgejo_id bigint NOT NULL,
    PRIMARY KEY (issue_id, node),
    UNIQUE (node, forgejo_id)
);

CREATE TABLE issue_comments (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id    uuid NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    origin_node text NOT NULL REFERENCES nodes (name),
    author      text NOT NULL,
    created_at  timestamptz NOT NULL,
    base_body   text NOT NULL,
    deleted_at  timestamptz
);
CREATE INDEX issue_comments_issue ON issue_comments (issue_id);

CREATE TABLE issue_comment_copies (
    comment_id uuid NOT NULL REFERENCES issue_comments (id) ON DELETE CASCADE,
    node       text NOT NULL REFERENCES nodes (name),
    forgejo_id bigint NOT NULL,
    PRIMARY KEY (comment_id, node),
    UNIQUE (node, forgejo_id)
);
