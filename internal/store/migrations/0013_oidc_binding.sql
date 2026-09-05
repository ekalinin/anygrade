-- A course may sit behind an identity provider (SPEC §8). What that adds is a
-- second way to obtain a session, not a second kind of account: the personal
-- token stays, because it is also the git-over-HTTP basic-auth password and git
-- has nowhere to run a browser redirect.
--
-- The binding is the provider's own subject identifier, `iss` + `sub`, and not
-- the email or the login it happens to be spelled with today. A university
-- renames a student, a person marries, a provider changes its username policy -
-- and none of it may cost that student their account or, worse, hand it to
-- whoever inherits the freed name. `sub` is the one claim OIDC guarantees is
-- stable and never reassigned within an issuer.
--
-- Both columns are NULL for an account that has never signed in through a
-- provider, which is every account that exists today. SQLite treats NULLs as
-- distinct inside a UNIQUE index, so all of them coexist while the index still
-- makes one subject reachable by at most one account: without it, a race
-- between two logins could bind one identity to two accounts and leave the
-- second login picking between them.
ALTER TABLE users ADD COLUMN oidc_iss TEXT;
ALTER TABLE users ADD COLUMN oidc_sub TEXT;
CREATE UNIQUE INDEX idx_users_oidc ON users(oidc_iss, oidc_sub);

-- sessions.token_hash was NOT NULL because every session was opened by
-- verifying a token, and binding to it is what makes a token reset revoke the
-- sessions it opened. A provider login opens a session without one: it was not
-- obtained from a token, and an account invited but never activated does not
-- even have a token to bind to. The column therefore becomes nullable, and NULL
-- means exactly "this session does not depend on a token".
--
-- The consequence is deliberate and worth stating: rotating a token no longer
-- signs out a session opened through the provider, because it never
-- authenticated that session. Deactivating the account still does - every
-- credential path filters on users.state - and that is the revocation lever.
--
-- SQLite cannot relax a NOT NULL in place, so the table is rebuilt. Rows are
-- copied rather than dropped: 0006 already signed everybody out once, and there
-- is no reason to do it again.
CREATE TABLE sessions_new (
  id_hash    TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
INSERT INTO sessions_new (id_hash, user_id, token_hash, created_at, expires_at)
  SELECT id_hash, user_id, token_hash, created_at, expires_at FROM sessions;
DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;
CREATE INDEX idx_sessions_user ON sessions(user_id);
