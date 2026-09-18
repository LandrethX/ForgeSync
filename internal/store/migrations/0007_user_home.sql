-- SceneID users and their primary site ("home"). A repository owned by a
-- SceneID user takes its owner's primary site as its primary.

-- One row per SceneID user, keyed by the OIDC subject.
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sub           text NOT NULL UNIQUE,
    login         text NOT NULL,
    home_node     text REFERENCES nodes (name),
    -- 'registration' (automatic: the node the account was created on first),
    -- 'manual' (an Administrator), or '' while none is set.
    home_source   text NOT NULL DEFAULT '' CHECK (home_source IN ('', 'registration', 'manual')),
    first_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX users_login ON users (lower(login));

-- What the last successful scan of a node saw of each user's account there.
CREATE TABLE user_accounts (
    user_id            uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    node               text NOT NULL REFERENCES nodes (name),
    login              text NOT NULL,
    forgejo_id         bigint NOT NULL,
    forgejo_created_at timestamptz,
    present            boolean NOT NULL,
    checked_at         timestamptz NOT NULL,
    PRIMARY KEY (user_id, node)
);

-- Accounts ForgeSync created itself. They never count as where a user
-- registered.
CREATE TABLE created_accounts (
    node       text NOT NULL REFERENCES nodes (name),
    login      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node, login)
);
INSERT INTO created_accounts (node, login, created_at)
SELECT DISTINCT ON (details->>'node', target) details->>'node', target, at
FROM audit_log
WHERE action = 'user.created_on_node' AND details->>'node' IN (SELECT name FROM nodes)
ORDER BY details->>'node', target, at;

-- A repository's primary can now also come from its owner: 'owner'.
ALTER TABLE repositories DROP CONSTRAINT repositories_primary_source_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_primary_source_check
    CHECK (primary_source IN ('', 'origin', 'owner', 'manual'));
