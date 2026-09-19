-- Every ForgeSync controller sharing this database says it's there, on
-- the same beat as the leadership lease. Without it a controller only
-- knows about itself and whoever holds the lease, so a standby that has
-- stopped is invisible until a failover needs it.
--
-- address is what the database sees the connection come from, so it's the
-- controller's real address rather than what it believes about itself.
CREATE TABLE controllers (
    name         text PRIMARY KEY,
    holder       text NOT NULL,
    url          text NOT NULL DEFAULT '',
    version      text NOT NULL DEFAULT '',
    address      text NOT NULL DEFAULT '',
    started_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL
);
