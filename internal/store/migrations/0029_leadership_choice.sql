-- An administrator's choice of which controller should be leading, when
-- the configured order isn't what's wanted at the moment: a maintenance
-- window on one of them, a controller closer to the nodes, an outage
-- somewhere. One row, or none at all, which means "follow the priority".
CREATE TABLE leadership_choice (
    one        boolean PRIMARY KEY DEFAULT true CHECK (one),
    controller text NOT NULL,
    chosen_by  text NOT NULL DEFAULT '',
    chosen_at  timestamptz NOT NULL DEFAULT now()
);
