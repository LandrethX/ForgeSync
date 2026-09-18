-- Nodes, their health, and the audit log. Sync state (repositories, object
-- mappings, events, conflicts) follows once the Phase 0 results are in.

CREATE TABLE nodes (
    name       text PRIMARY KEY,
    url        text NOT NULL,
    site       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Latest health of each node (one row per node).
CREATE TABLE node_status (
    node                 text PRIMARY KEY REFERENCES nodes (name),
    state                text NOT NULL,
    version              text NOT NULL DEFAULT '',
    last_checked         timestamptz NOT NULL,
    last_seen            timestamptz,
    failing_since        timestamptz,
    consecutive_failures integer NOT NULL DEFAULT 0,
    last_error           text NOT NULL DEFAULT ''
);

-- Every change of health state, for availability history.
CREATE TABLE node_state_transitions (
    id         bigserial PRIMARY KEY,
    node       text NOT NULL REFERENCES nodes (name),
    from_state text NOT NULL,
    to_state   text NOT NULL,
    at         timestamptz NOT NULL,
    error      text NOT NULL DEFAULT ''
);
CREATE INDEX node_state_transitions_node_at ON node_state_transitions (node, at);

CREATE TABLE audit_log (
    id      bigserial PRIMARY KEY,
    at      timestamptz NOT NULL DEFAULT now(),
    actor   text NOT NULL,
    action  text NOT NULL,
    target  text NOT NULL DEFAULT '',
    details jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_at ON audit_log (at);
