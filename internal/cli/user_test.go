package cli

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ekalinin/anygrade/internal/store"
)

func TestParseRoster(t *testing.T) {
	t.Run("header detection and trim", func(t *testing.T) {
		csv := "login,display_name\n alice , Alice A \nbob,\n"
		got, err := parseRoster(strings.NewReader(csv))
		if err != nil {
			t.Fatal(err)
		}
		want := []rosterEntry{{Login: "alice", Name: "Alice A"}, {Login: "bob", Name: ""}}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("no header", func(t *testing.T) {
		got, err := parseRoster(strings.NewReader("alice,Alice A\nbob,Bob B\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Login != "alice" || got[1].Login != "bob" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("extra columns ignored", func(t *testing.T) {
		got, err := parseRoster(strings.NewReader("alice,Alice A,extra,more\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Login != "alice" || got[0].Name != "Alice A" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("invalid login reports 1-based row number", func(t *testing.T) {
		_, err := parseRoster(strings.NewReader("login\nalice\nBad Login\nbob\n"))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "row 3:") {
			t.Errorf("error = %q, want it to mention row 3", err)
		}
	})

	t.Run("empty login reports its row number", func(t *testing.T) {
		_, err := parseRoster(strings.NewReader("alice\n,Nobody\nbob\n"))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "row 2:") {
			t.Errorf("error = %q, want it to mention row 2", err)
		}
	})
}

// testAuthorizedKey returns one throwaway authorized_keys line.
func testAuthorizedKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

// TestUserAddKeyIsUnproven: the teacher CLI is the one registration path
// without proof of possession (SPEC §8). It must not claim a proof it never
// saw, or the flag a teacher reads on the student page would be a lie.
func TestUserAddKeyIsUnproven(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := db.CreateUser(t.Context(), "alice", "Alice", "student")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	key := testAuthorizedKey(t)
	if err := userAddKey([]string{"--login", "alice", "--key", key, "--data-dir", dir}); err != nil {
		t.Fatalf("add-key: %v", err)
	}

	db, err = store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, err := db.ListSSHKeys(t.Context(), u.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListSSHKeys = %v (err %v), want one key", keys, err)
	}
	if keys[0].VerifiedAt != nil {
		t.Errorf("verified_at = %v, want nil", keys[0].VerifiedAt)
	}
}

// TestUserAddKeyNamesTheHolder: a contested fingerprint is exactly what a
// teacher has to resolve, and a raw UNIQUE constraint says nothing about who
// to talk to.
func TestUserAddKeyNamesTheHolder(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{"alice", "bob"} {
		if _, cerr := db.CreateUser(t.Context(), login, "", "student"); cerr != nil {
			t.Fatal(cerr)
		}
	}
	db.Close()

	key := testAuthorizedKey(t)
	if err := userAddKey([]string{"--login", "alice", "--key", key, "--data-dir", dir}); err != nil {
		t.Fatalf("add-key: %v", err)
	}
	err = userAddKey([]string{"--login", "bob", "--key", key, "--data-dir", dir})
	if err == nil || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("err = %v, want it to name the holder alice", err)
	}
}
