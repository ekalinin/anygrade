-- SSH key registration proved nothing about possession. Public keys are
-- public and ssh_keys.fingerprint is globally unique, so posting a classmate's
-- key refused them their own ("already registered") and made their SSH
-- connections resolve to the squatter - a working denial of service against a
-- named student. A key is now registered only after its holder signs a server
-- challenge with the private half (SPEC §8).
--
-- verified_at records that proof. It stays NULL for every key registered
-- before this migration and for keys a teacher adds out of band with
-- `anygrade user add-key`. Those keys keep authenticating: re-proving all of
-- them at once would lock a running course out of SSH over a hole that is
-- denial of service only, and already detected and audited. They are shown as
-- unproven instead, and a proven registration may take a contested fingerprint
-- back from an unproven one - which is what lets an existing squat heal
-- without waiting for a teacher.
ALTER TABLE ssh_keys ADD COLUMN verified_at TEXT;

-- The challenge nonce is a credential, so the table stores only its hash, like
-- tokens, invites and session ids: reading the database gives no usable proof.
--
-- UNIQUE(user_id) keeps at most one live challenge per account - issuing a new
-- one replaces the old - so the table is bounded by accounts rather than by
-- attempts, and a student who reloads the form cannot leave a trail of valid
-- nonces behind. Consuming a challenge deletes its row, which is what makes it
-- single-use and what keeps expired rows from accumulating.
CREATE TABLE ssh_key_challenges (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  nonce_hash  TEXT    NOT NULL UNIQUE,
  fingerprint TEXT    NOT NULL,
  public_key  TEXT    NOT NULL,
  created_at  TEXT    NOT NULL,
  expires_at  TEXT    NOT NULL
);
