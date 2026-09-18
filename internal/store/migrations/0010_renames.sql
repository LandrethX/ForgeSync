-- Renames and transfers on a repository's primary, detected by Forgejo's
-- repository id (which a rename or transfer keeps).

-- The name each node's copy actually has. It differs from the repository's
-- name while ForgeSync hasn't renamed that copy yet.
ALTER TABLE repository_replicas ADD COLUMN full_name text NOT NULL DEFAULT '';
UPDATE repository_replicas rr SET full_name = r.full_name FROM repositories r WHERE r.id = rr.repository_id;

-- Old names of renamed repositories. A copy found under an old name on a
-- node other than the primary belongs to the renamed repository. An alias
-- goes once no node has a copy under it.
CREATE TABLE repository_aliases (
    name          text PRIMARY KEY, -- lower(owner/name)
    repository_id uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now()
);
