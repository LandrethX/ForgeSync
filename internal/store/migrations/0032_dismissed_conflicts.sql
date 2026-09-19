-- Some conflicts aren't ForgeSync's to resolve and aren't going away:
-- an Actions secret it can't copy, a difference someone has decided to
-- live with. Acknowledging one leaves a note but keeps it counted, so
-- the dashboard shows a number nobody can ever bring to zero.
--
-- Dismissing says "we know, stop counting this". The conflict is kept,
-- with who dismissed it and why; it simply isn't open any more.
ALTER TABLE conflicts ADD COLUMN dismissed_at timestamptz;
ALTER TABLE conflicts ADD COLUMN dismissed_by text NOT NULL DEFAULT '';

-- A dismissed conflict still occupies its (repository, kind, ref), so
-- the next check updates that row instead of opening a second one.
DROP INDEX conflicts_open;
CREATE UNIQUE INDEX conflicts_live ON conflicts (repository_id, kind, ref)
    WHERE state IN ('open', 'dismissed');
