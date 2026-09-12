-- One live invite per account.
--
-- CreateInvite was a plain INSERT and the schema held only token_hash unique,
-- so `anygrade user invite` re-run over a roster left every account it had
-- already invited with a second working link. Each of those links activates:
-- it issues a token, which replaces the one the student is using, and opens a
-- session as them - so a link meant for a student who never received the first
-- one hands their account to whoever opens it instead.
--
-- The UNIQUE index makes a re-invite an upsert on user_id: the account's
-- outstanding link is replaced rather than joined, which is also what the
-- legitimate case wants - a first link that expired or was lost. Accounts that
-- already carry several keep the newest, the one the teacher last handed out.
DELETE FROM invites WHERE id NOT IN (SELECT MAX(id) FROM invites GROUP BY user_id);
CREATE UNIQUE INDEX idx_invites_user ON invites(user_id);
