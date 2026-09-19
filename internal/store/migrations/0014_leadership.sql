-- One controller acts at a time. The lease is held in the database the
-- controllers share, so the database is the witness: a controller acts only
-- while it holds an unexpired lease, and another can take it over only
-- after that lease has run out.
CREATE TABLE leadership (
    id          boolean PRIMARY KEY DEFAULT true CHECK (id), -- one row
    holder      text NOT NULL,        -- the running controller's instance id
    name        text NOT NULL,        -- its configured name, for people
    url         text NOT NULL,        -- where to reach it, or ''
    acquired_at timestamptz NOT NULL, -- when this holder took over
    renewed_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL
);
