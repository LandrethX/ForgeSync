-- Every repository gets a primary. Unless an Administrator picks one, it's the
-- node where the repository was created first (its origin), which needs each
-- node's creation time.
ALTER TABLE repository_replicas ADD COLUMN forgejo_created_at timestamptz;

-- How the primary was chosen: 'origin' (automatic), 'manual' (an
-- Administrator), or '' while none is set.
ALTER TABLE repositories ADD COLUMN primary_source text NOT NULL DEFAULT ''
    CHECK (primary_source IN ('', 'origin', 'manual'));
UPDATE repositories SET primary_source = 'manual' WHERE primary_node IS NOT NULL;
