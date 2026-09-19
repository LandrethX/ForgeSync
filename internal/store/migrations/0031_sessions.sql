-- Signed-in sessions. They used to live in the controller's memory, which
-- meant a failover signed everyone out: the work moved to the other
-- controller and the person watching it had to sign in again, at exactly
-- the moment they were paying attention. Both controllers share this
-- database, so a session kept here survives the move.
--
-- The cookie's value isn't stored, only its SHA-256: a dump of this table
-- doesn't hand anyone a signed-in session.
CREATE TABLE sessions (
    key_hash     text PRIMARY KEY,
    identity     jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX sessions_expires ON sessions (expires_at);
