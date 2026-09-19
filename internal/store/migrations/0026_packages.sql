-- What the nodes last agreed one owner's packages are: the members
-- "<type>|<name>|<version>|<file>|<digest>" the merge works from.
-- Packages belong to an owner (a person or an organization), not to a
-- repository, so this is keyed by the owner's name, as organizations are.
CREATE TABLE package_owners (
    owner         text PRIMARY KEY,
    base_packages text NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now()
);
