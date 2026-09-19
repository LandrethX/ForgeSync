-- Pull requests take part in issue replication as a kind of issue: the
-- same identity and copies, the same merged conversation. What's extra is
-- which branches they're between, so a copy can be opened on another node,
-- and the flag that says which records these are. Their state is never
-- merged: ForgeSync closes a copy when the primary's is closed or merged,
-- and never merges or reopens one, because merging twice makes two
-- different merge commits.
ALTER TABLE issues ADD COLUMN is_pull boolean NOT NULL DEFAULT false;
ALTER TABLE issues ADD COLUMN head_branch text NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN base_branch text NOT NULL DEFAULT '';
