-- The releases the nodes last agreed on, as "<tag>:<digest of what it
-- says>" members, and their files as "<tag>|<size>|<name>". A tag means
-- the same thing on every node -- git replication put it there -- so it is
-- what identifies a release.
ALTER TABLE repositories ADD COLUMN base_releases text NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN base_release_assets text NOT NULL DEFAULT '';
