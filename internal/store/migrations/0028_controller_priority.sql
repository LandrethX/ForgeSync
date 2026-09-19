-- Which controller should lead when more than one can. Lower goes first,
-- so a pair configured 1 and 2 always settles on the same one, and the
-- other only acts while it's away. 0 (the default) means no preference,
-- which is the behaviour this had before: whoever holds the lease keeps it.
ALTER TABLE controllers ADD COLUMN priority integer NOT NULL DEFAULT 0;
