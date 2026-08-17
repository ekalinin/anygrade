-- Two guarantees the Go code alone cannot give.
--
-- 1. Sessions. `sessions.id` was the cookie value stored verbatim, so anybody
--    who could read anygrade.db held a ready-to-use cookie for every live
--    session, teachers included - while the tokens right next to it were
--    hashed. The column now holds sha256(cookie), like tokens and invites.
--    A hash cannot be derived from the rows already stored, so they go: the
--    upgrade signs everybody out once.
--
-- 2. One token per user. IssueToken deleted and inserted in two statements
--    and nothing in the schema held it to one row, so two rotations racing
--    each other (a student regenerating while the teacher resets) could both
--    delete and then both insert, leaving two valid PATs for one account.
--    The UNIQUE index makes the pair a single upsert. Accounts that already
--    carry duplicates keep their newest token.

DELETE FROM sessions;
ALTER TABLE sessions RENAME COLUMN id TO id_hash;

DELETE FROM tokens WHERE id NOT IN (SELECT MAX(id) FROM tokens GROUP BY user_id);
DROP INDEX idx_tokens_user;
CREATE UNIQUE INDEX idx_tokens_user ON tokens(user_id);
