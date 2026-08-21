package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

const testNonce = "agc_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func secondUser(t *testing.T, db *DB) User {
	t.Helper()
	u, err := db.CreateUser(t.Context(), "student2", "Student Two", "student")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestKeyChallengeRoundTrip: issue, read back the key it was issued for, burn.
func TestKeyChallengeRoundTrip(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	if err := db.CreateKeyChallenge(t.Context(), u.ID, testNonce,
		"SHA256:aaa", "ssh-ed25519 AAA a", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	c, ok, err := db.LookupKeyChallenge(t.Context(), testNonce, time.Now())
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if c.UserID != u.ID || c.Fingerprint != "SHA256:aaa" || c.PublicKey != "ssh-ed25519 AAA a" {
		t.Fatalf("challenge = %+v", c)
	}
	if _, ok, err := db.LookupKeyChallenge(t.Context(), "agc_nope", time.Now()); err != nil || ok {
		t.Fatalf("unknown nonce: ok=%v err=%v", ok, err)
	}

	used, err := db.ConsumeKeyChallenge(t.Context(), testNonce)
	if err != nil || !used {
		t.Fatalf("consume: used=%v err=%v", used, err)
	}
	if _, ok, err := db.LookupKeyChallenge(t.Context(), testNonce, time.Now()); err != nil || ok {
		t.Fatalf("nonce still live after use: ok=%v err=%v", ok, err)
	}
}

// TestKeyChallengeExpires: a nonce past its window is no different from an
// unknown one, and reading it does not resurrect it.
func TestKeyChallengeExpires(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	if err := db.CreateKeyChallenge(t.Context(), u.ID, testNonce,
		"SHA256:aaa", "ssh-ed25519 AAA a", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.LookupKeyChallenge(t.Context(), testNonce, time.Now()); err != nil || ok {
		t.Fatalf("expired nonce: ok=%v err=%v", ok, err)
	}
}

// TestConsumeKeyChallengeIsAtomic: the nonce is a credential, so exactly one
// of several racing consumers may win - the same guarantee ConsumeInvite gives
// the one-shot activation link.
func TestConsumeKeyChallengeIsAtomic(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	if err := db.CreateKeyChallenge(t.Context(), u.ID, testNonce,
		"SHA256:aaa", "ssh-ed25519 AAA a", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	const n = 8
	results := make([]bool, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			used, err := db.ConsumeKeyChallenge(t.Context(), testNonce)
			if err != nil {
				t.Errorf("consume: %v", err)
			}
			results[i] = used
		})
	}
	wg.Wait()

	won := 0
	for _, r := range results {
		if r {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d consumers burned the nonce, want exactly 1", won)
	}
}

// TestCreateKeyChallengeReplacesTheOutstandingOne: one live challenge per
// account, so reloading the form does not leave a trail of valid nonces.
func TestCreateKeyChallengeReplacesTheOutstandingOne(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	const second = "agc_" + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	for _, n := range []string{testNonce, second} {
		if err := db.CreateKeyChallenge(t.Context(), u.ID, n,
			"SHA256:aaa", "ssh-ed25519 AAA a", time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	if _, ok, err := db.LookupKeyChallenge(t.Context(), testNonce, time.Now()); err != nil || ok {
		t.Fatalf("the superseded nonce is still live: ok=%v err=%v", ok, err)
	}
	if _, ok, err := db.LookupKeyChallenge(t.Context(), second, time.Now()); err != nil || !ok {
		t.Fatalf("the current nonce is not live: ok=%v err=%v", ok, err)
	}
}

// TestAddProvenSSHKeyStampsProof: the proven path is what the settings flow
// writes, and the stamp is what protects the row from a later takeover.
func TestAddProvenSSHKeyStampsProof(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	k, displaced, err := db.AddProvenSSHKey(t.Context(), u.ID, "SHA256:aaa", "ssh-ed25519 AAA a")
	if err != nil || displaced != nil {
		t.Fatalf("add: displaced=%+v err=%v", displaced, err)
	}
	if k.VerifiedAt == nil {
		t.Fatal("verified_at was not stamped")
	}
}

// TestAddSSHKeyLeavesProofUnset: `anygrade user add-key` is an out-of-band
// trust path and must never claim a proof the server never saw.
func TestAddSSHKeyLeavesProofUnset(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	k, err := db.AddSSHKey(t.Context(), u.ID, "SHA256:aaa", "ssh-ed25519 AAA a")
	if err != nil {
		t.Fatal(err)
	}
	if k.VerifiedAt != nil {
		t.Fatalf("verified_at = %v, want nil", k.VerifiedAt)
	}
}

// TestAddProvenSSHKeyUpgradesOwnKey: proving a key you already registered
// unproven stamps the existing row instead of failing on UNIQUE(fingerprint).
func TestAddProvenSSHKeyUpgradesOwnKey(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)

	legacy, err := db.AddSSHKey(t.Context(), u.ID, "SHA256:aaa", "ssh-ed25519 AAA a")
	if err != nil {
		t.Fatal(err)
	}
	k, displaced, err := db.AddProvenSSHKey(t.Context(), u.ID, "SHA256:aaa", "ssh-ed25519 AAA a")
	if err != nil || displaced != nil {
		t.Fatalf("upgrade: displaced=%+v err=%v", displaced, err)
	}
	if k.ID != legacy.ID {
		t.Errorf("key id %d -> %d: the row was replaced, not upgraded", legacy.ID, k.ID)
	}
	if k.VerifiedAt == nil {
		t.Error("verified_at was not stamped on the upgrade")
	}
	keys, err := db.ListSSHKeys(t.Context(), u.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys = %v (err %v), want exactly one row", keys, err)
	}
}

// TestAddProvenSSHKeyRefusesProvenHolder: a proof does not beat a proof, or
// the fingerprint would just be first-come-first-served again.
func TestAddProvenSSHKeyRefusesProvenHolder(t *testing.T) {
	db := openTestDB(t)
	holder, other := testUser(t, db), secondUser(t, db)

	if _, _, err := db.AddProvenSSHKey(t.Context(), holder.ID, "SHA256:aaa", "ssh-ed25519 AAA a"); err != nil {
		t.Fatal(err)
	}
	_, _, err := db.AddProvenSSHKey(t.Context(), other.ID, "SHA256:aaa", "ssh-ed25519 AAA a")
	if !errors.Is(err, ErrKeyHeld) {
		t.Fatalf("err = %v, want ErrKeyHeld", err)
	}
	if keys, kerr := db.ListSSHKeys(t.Context(), holder.ID); kerr != nil || len(keys) != 1 {
		t.Fatalf("the holder lost the key: %v (err %v)", keys, kerr)
	}
}

// TestAddProvenSSHKeyDisplacesUnprovenHolder: the deliberate change of who
// wins. Only the private key can produce a proof, so an unproven row - legacy
// or teacher-added - loses to it, and the losing account is reported so the
// takeover can be audited.
func TestAddProvenSSHKeyDisplacesUnprovenHolder(t *testing.T) {
	db := openTestDB(t)
	squatter, owner := testUser(t, db), secondUser(t, db)

	if _, err := db.AddSSHKey(t.Context(), squatter.ID, "SHA256:aaa", "ssh-ed25519 AAA a"); err != nil {
		t.Fatal(err)
	}
	k, displaced, err := db.AddProvenSSHKey(t.Context(), owner.ID, "SHA256:aaa", "ssh-ed25519 AAA a")
	if err != nil {
		t.Fatal(err)
	}
	if displaced == nil || displaced.Login != squatter.Login {
		t.Fatalf("displaced = %+v, want %q", displaced, squatter.Login)
	}
	if k.UserID != owner.ID || k.VerifiedAt == nil {
		t.Fatalf("key = %+v, want a proven key for %q", k, owner.Login)
	}
	if keys, kerr := db.ListSSHKeys(t.Context(), squatter.ID); kerr != nil || len(keys) != 0 {
		t.Fatalf("the squatter kept the key: %v (err %v)", keys, kerr)
	}
	got, ok, err := db.UserByFingerprint(t.Context(), "SHA256:aaa")
	if err != nil || !ok || got.ID != owner.ID {
		t.Fatalf("fingerprint resolves to %+v (ok=%v err=%v), want %q", got, ok, err, owner.Login)
	}
}
