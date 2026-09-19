-- ForgeSync's own accounts. They exist here and nowhere else: they are
-- not Forgejo users, nothing replicates them to a node, and a node never
-- learns they exist. Both controllers share this database, so an account
-- made on either works on both, which is what makes them usable during a
-- failover -- the moment SceneID or one controller is the thing that's
-- broken.
--
-- The password is stored as a PBKDF2-SHA256 hash with its own salt and
-- iteration count in the string, so the cost can be raised later without
-- invalidating what's already here.
CREATE TABLE accounts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text NOT NULL,
    password_hash text NOT NULL,
    full_name     text NOT NULL DEFAULT '',
    role          text NOT NULL,
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    text NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_sign_in  timestamptz
);
-- One account per name, whatever case it was typed in.
CREATE UNIQUE INDEX accounts_username ON accounts (lower(username));
