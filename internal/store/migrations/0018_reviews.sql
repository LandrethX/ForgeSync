-- A pull request's reviews, as last agreed everywhere: the digest of each
-- submitted review with its line comments, comma-separated. Nothing in a
-- review is a node's own -- the reviewer, the commit it is against and
-- every word are the same wherever it was written -- so the digest
-- identifies it across the nodes. As with reactions and attachments, one
-- enters the base only once every node has it and leaves only once none
-- has.
ALTER TABLE issues ADD COLUMN base_reviews text NOT NULL DEFAULT '';
