-- What the nodes last agreed an organization looks like: its profile
-- fields, its teams and who is in them. An organization's name is its
-- identity everywhere, as a login is. Teams are "<team>:<permission>" and
-- members "<team>:<login>", merged member by member like everything else
-- of that shape.
CREATE TABLE organizations (
    name         text PRIMARY KEY,
    base_fields  jsonb NOT NULL DEFAULT '{}',
    base_teams   text NOT NULL DEFAULT '',
    base_members text NOT NULL DEFAULT '',
    updated_at   timestamptz NOT NULL DEFAULT now()
);
