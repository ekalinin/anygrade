-- The TA role (SPEC §8): a course assistant reviews work - submissions, code,
-- logs, the queue - without the account management half a teacher has.
--
-- users.role is CHECK-constrained, and SQLite can only widen a CHECK by
-- rebuilding the table, so this is the rebuild its own documentation
-- prescribes: a new table, the rows copied across, the old one dropped, the
-- new one renamed into its place.
--
-- Both of those steps depend on foreign keys being off, which is why migrate()
-- runs the whole ledger that way. With enforcement on, DROP TABLE users
-- performs an implicit DELETE FROM first and fires the ON DELETE CASCADE of
-- every table that references it - tokens, sessions, ssh_keys, invites,
-- challenges - and the RENAME rewrites those REFERENCES clauses to point at
-- the temporary name. Off, the children keep referring to `users` and find the
-- new table under it.
CREATE TABLE users_new (
  id           INTEGER PRIMARY KEY,
  login        TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL DEFAULT '',
  role         TEXT NOT NULL CHECK (role IN ('student','ta','teacher')),
  state        TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','disabled')),
  created_at   TEXT NOT NULL
);

INSERT INTO users_new (id, login, display_name, role, state, created_at)
  SELECT id, login, display_name, role, state, created_at FROM users;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
