-- Identities: what a machine is to other services. Each is a ServiceAccount of the namespace,
-- identity-<name>-<random>, whose tokens say system:serviceaccount:<namespace>:<account>. A member
-- owns it and may have several machines wear it; every machine wears exactly one.
CREATE TABLE identities (
  account    text PRIMARY KEY,          -- the ServiceAccount's name; never reused (retired_identities)
  owner      text NOT NULL REFERENCES members (owner),
  name       text NOT NULL,             -- the member's name for it
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (owner, name)
);

-- An account once used is never made again: a grant to it can only ever mean that identity.
CREATE TABLE retired_identities (
  account    text PRIMARY KEY,
  retired_at timestamptz NOT NULL DEFAULT now()
);

-- NULL only for machines from before, until the service gives them one at its start. The key keeps
-- an identity from being deleted while a machine wears it.
ALTER TABLE machines ADD COLUMN identity text REFERENCES identities (account);
ALTER TABLE machines ADD COLUMN delete_identity boolean NOT NULL DEFAULT false; -- with the machine, if no other wears it
CREATE INDEX machines_identity ON machines (identity);
