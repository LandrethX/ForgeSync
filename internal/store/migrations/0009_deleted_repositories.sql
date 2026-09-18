-- Repositories deleted on their primary. ForgeSync archives the copies on
-- the other nodes (renamed, moved into a private archive organization and
-- marked archived) and deletes them after the backup period.
ALTER TABLE repositories ADD COLUMN deleted_at timestamptz;

CREATE TABLE repository_archives (
    id            bigserial PRIMARY KEY,
    repository_id uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    node          text NOT NULL REFERENCES nodes (name),
    original_name text NOT NULL, -- owner/name before archiving
    archived_name text NOT NULL, -- the name in the archive organization
    -- renamed: renamed in place, not moved yet. archived: in the archive
    -- organization. purged: deleted after the backup period.
    state         text NOT NULL CHECK (state IN ('renamed', 'archived', 'purged')),
    archived_at   timestamptz NOT NULL,
    delete_after  timestamptz,
    UNIQUE (node, archived_name)
);
CREATE INDEX repository_archives_repo ON repository_archives (repository_id);
