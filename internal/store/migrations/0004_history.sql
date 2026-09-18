-- Support for the combined history (audit log + node health changes),
-- which is ordered and filtered by time.
CREATE INDEX node_state_transitions_at ON node_state_transitions (at);
CREATE INDEX audit_log_actor ON audit_log (actor);
