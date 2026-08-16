-- Timestamps were written with time.RFC3339Nano, which drops trailing zeros of
-- the fractional part. The columns are TEXT and SQLite compares them
-- lexicographically, so two timestamps inside one second sort backwards
-- whenever the earlier one is a prefix of the later one:
--
--   earlier  2026-08-15T10:00:00.387Z
--   later    2026-08-15T10:00:00.387026Z
--
-- and 'Z' (0x5A) > '0' (0x30). That inverts ClaimNext's execution order, the
-- cooldown anchor taken from ListByUserTask, and every "latest submission"
-- lookup. Rows are now written with nine mandatory digits; rewrite the ones
-- already stored, or old and new rows would compare even worse than before.
--
-- The padding expression: strip the trailing 'Z', add the '.' when the value
-- carries no fractional part at all, append nine zeros, then cut back to the
-- fixed 19 + 1 + 9 characters and put the 'Z' back. The guard skips values
-- that are already 30 characters wide, which also makes the migration
-- idempotent, and anything outside the RFC 3339 width range, including NULL.

UPDATE users SET created_at =
  substr(rtrim(created_at, 'Z') || CASE WHEN instr(created_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(created_at) BETWEEN 20 AND 29;

UPDATE tokens SET created_at =
  substr(rtrim(created_at, 'Z') || CASE WHEN instr(created_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(created_at) BETWEEN 20 AND 29;

UPDATE tokens SET last_used_at =
  substr(rtrim(last_used_at, 'Z') || CASE WHEN instr(last_used_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(last_used_at) BETWEEN 20 AND 29;

UPDATE ssh_keys SET created_at =
  substr(rtrim(created_at, 'Z') || CASE WHEN instr(created_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(created_at) BETWEEN 20 AND 29;

UPDATE invites SET expires_at =
  substr(rtrim(expires_at, 'Z') || CASE WHEN instr(expires_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(expires_at) BETWEEN 20 AND 29;

UPDATE invites SET used_at =
  substr(rtrim(used_at, 'Z') || CASE WHEN instr(used_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(used_at) BETWEEN 20 AND 29;

UPDATE submissions SET received_at =
  substr(rtrim(received_at, 'Z') || CASE WHEN instr(received_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(received_at) BETWEEN 20 AND 29;

UPDATE submissions SET retry_at =
  substr(rtrim(retry_at, 'Z') || CASE WHEN instr(retry_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(retry_at) BETWEEN 20 AND 29;

UPDATE submissions SET started_at =
  substr(rtrim(started_at, 'Z') || CASE WHEN instr(started_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(started_at) BETWEEN 20 AND 29;

UPDATE submissions SET canceled_at =
  substr(rtrim(canceled_at, 'Z') || CASE WHEN instr(canceled_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(canceled_at) BETWEEN 20 AND 29;

UPDATE score_overrides SET created_at =
  substr(rtrim(created_at, 'Z') || CASE WHEN instr(created_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(created_at) BETWEEN 20 AND 29;

UPDATE events SET created_at =
  substr(rtrim(created_at, 'Z') || CASE WHEN instr(created_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(created_at) BETWEEN 20 AND 29;

UPDATE sessions SET created_at =
  substr(rtrim(created_at, 'Z') || CASE WHEN instr(created_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(created_at) BETWEEN 20 AND 29;

UPDATE sessions SET expires_at =
  substr(rtrim(expires_at, 'Z') || CASE WHEN instr(expires_at, '.') = 0 THEN '.' ELSE '' END || '000000000', 1, 29) || 'Z'
WHERE length(expires_at) BETWEEN 20 AND 29;
