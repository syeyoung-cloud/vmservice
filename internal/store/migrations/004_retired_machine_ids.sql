-- A machine's ID is its identity's subject (system:serviceaccount:<namespace>:<id>), which members
-- grant things to. An ID once used is never given to another machine, so no later machine is ever
-- what an old grant named.
CREATE TABLE retired_machine_ids (
    id         text PRIMARY KEY,
    retired_at timestamptz NOT NULL DEFAULT now()
);
