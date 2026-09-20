-- Nodes move into ForgeSync's own database, so one can be added from the
-- admin UI and every controller sees it without a file being edited on
-- each of them. What a node needs beyond its address was until now only in
-- the config file: which local admin the token belongs to, the id of the
-- SceneID login source there, and the token itself.
--
-- The token is sealed (internal/secret, AES-256-GCM) with a key each
-- controller holds on disk and the database never sees. A dump, a backup
-- or a streaming standby is therefore not a set of site-admin tokens for
-- every Forgejo node. A null sealed_token means the node is still taking
-- its token from the config file, which is what every node does until it
-- is imported.
--
-- source says where the node came from: 'config' for one read out of the
-- config file, 'api' for one added through the admin API. It is for people
-- reading the table, not for logic.
--
-- removed_at retires a node instead of deleting the row. Its health
-- transitions are part of the history, which nothing may edit, and they
-- reference this table; a node taken out and put back should also keep
-- what is known about it. A retired node is not watched, not scanned and
-- not replicated to, and adding it again clears the mark.

ALTER TABLE nodes
    ADD COLUMN service_user      text   NOT NULL DEFAULT 'forgesync',
    ADD COLUMN sceneid_source_id bigint NOT NULL DEFAULT 0,
    ADD COLUMN sealed_token      bytea,
    ADD COLUMN source            text   NOT NULL DEFAULT 'config',
    ADD COLUMN added_by          text   NOT NULL DEFAULT '',
    ADD COLUMN removed_at        timestamptz;
