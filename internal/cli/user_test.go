package cli

import (
	"crypto/ed25519"
	"io"
	"os"
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

// TestUserAddAcceptsEveryRole: --role takes the TA from day one, since a role
// that can only be set by editing the database is not shipped (SPEC §8). The
// value is checked before the INSERT, so a typo is a message rather than a
// CHECK violation - and it leaves no account behind.
func TestUserAddAcceptsEveryRole(t *testing.T) {
	dir := t.TempDir()
	for _, role := range []string{store.RoleStudent, store.RoleTA, store.RoleTeacher} {
		if err := userAdd([]string{"--login", "u-" + role, "--role", role, "--data-dir", dir}); err != nil {
			t.Fatalf("user add --role %s: %v", role, err)
		}
	}
	// The invite path takes the same roster of roles.
	if err := userInvite([]string{"--login", "ta2", "--role", store.RoleTA, "--data-dir", dir}); err != nil {
		t.Fatalf("user invite --role ta: %v", err)
	}
	err := userAdd([]string{"--login", "nope", "--role", "assistant", "--data-dir", dir})
	if err == nil || !strings.Contains(err.Error(), "student, ta or teacher") {
		t.Fatalf("err = %v, want the accepted roles named", err)
	}

	db, oerr := store.Open(t.Context(), dir)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer db.Close()
	ta, gerr := db.GetUserByLogin(t.Context(), "u-ta")
	if gerr != nil {
		t.Fatalf("the TA was not created: %v", gerr)
	}
	if !ta.CanReview() || ta.CanAdminister() {
		t.Errorf("stored TA has the wrong rights: %+v", ta)
	}
	if _, gerr := db.GetUserByLogin(t.Context(), "nope"); gerr == nil {
		t.Error("the rejected role still created an account")
	}
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

// seedUser creates one student in a fresh data dir and returns the dir.
func seedUser(t *testing.T, login string) string {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateUser(t.Context(), login, "", store.RoleStudent); err != nil {
		t.Fatal(err)
	}
	return dir
}

// readState returns a user's state and every user.state event in the data dir,
// newest first - all of them, so a row written against some other login is
// visible too. It opens the data dir the way a second process would, which is
// the only way to see what the command actually committed.
func readState(t *testing.T, dir, login string) (string, []store.EventRow) {
	t.Helper()
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, err := db.GetUserByLogin(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	events, err := db.ListEvents(t.Context(), "user.state", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	return u.State, events
}

// captureStderr runs f with os.Stderr replaced by a pipe and returns what was
// written there. The usage line and the deprecation note go to the real
// os.Stderr, so this is the only way to read them back.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = saved
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	return string(out)
}

// TestUserDeactivateReactivate: both directions change the state and both leave
// the audit trail the teacher UI leaves for the same action (SPEC §8, §11). The
// actor is empty on purpose - the CLI has no session, so the row names nobody
// rather than the account it targets - and the detail is the new state, so one
// filter finds every state change whichever surface took it.
func TestUserDeactivateReactivate(t *testing.T) {
	dir := seedUser(t, "alice")

	for _, tc := range []struct{ cmd, want string }{
		{"deactivate", "disabled"},
		{"reactivate", "active"},
	} {
		if code := cmdUser([]string{tc.cmd, "--login", "alice", "--data-dir", dir}); code != 0 {
			t.Fatalf("user %s: exit code %d, want 0", tc.cmd, code)
		}
		state, events := readState(t, dir, "alice")
		if state != tc.want {
			t.Fatalf("after %s the state is %q, want %q", tc.cmd, state, tc.want)
		}
		if len(events) == 0 || events[0].Detail != tc.want {
			t.Fatalf("after %s the newest user.state event is %+v, want detail %q",
				tc.cmd, events, tc.want)
		}
		if events[0].ActorLogin != "" || events[0].ActorRole != "" {
			t.Errorf("after %s the event names actor %q (role %q), want nobody",
				tc.cmd, events[0].ActorLogin, events[0].ActorRole)
		}
	}

	// Both halves are on record, not just the last one.
	if _, events := readState(t, dir, "alice"); len(events) != 2 {
		t.Fatalf("%d user.state events, want one per direction", len(events))
	}
}

// TestUserRemoveIsAHiddenAlias: `remove` never removed anything, but scripts
// call it, so it keeps deactivating - audit event included - while the usage
// line offers only the name that says what happens. The note that it is
// deprecated goes to stderr, which leaves parsed output alone.
func TestUserRemoveIsAHiddenAlias(t *testing.T) {
	dir := seedUser(t, "alice")

	var code int
	note := captureStderr(t, func() {
		code = cmdUser([]string{"remove", "--login", "alice", "--data-dir", dir})
	})
	if code != 0 {
		t.Fatalf("user remove: exit code %d, want 0", code)
	}
	if !strings.Contains(note, "deprecated") || !strings.Contains(note, "deactivate") {
		t.Errorf("stderr = %q, want a deprecation note pointing at deactivate", note)
	}

	state, events := readState(t, dir, "alice")
	if state != "disabled" {
		t.Fatalf("state after the alias = %q, want disabled", state)
	}
	if len(events) != 1 || events[0].Detail != "disabled" || events[0].ActorLogin != "" {
		t.Fatalf("events after the alias = %+v, want one actorless disabled row", events)
	}

	usage := captureStderr(t, func() { cmdUser([]string{"bogus"}) })
	if strings.Contains(usage, "remove") {
		t.Errorf("the usage line still advertises remove: %q", usage)
	}
	if !strings.Contains(usage, "deactivate") || !strings.Contains(usage, "reactivate") {
		t.Errorf("the usage line names neither half of the pair: %q", usage)
	}
}

// TestUserSetStateUnknownLogin: a login that matches nothing is a message, not
// a panic and not a silent success - and it leaves no audit row claiming a
// state change that never happened.
func TestUserSetStateUnknownLogin(t *testing.T) {
	dir := seedUser(t, "alice")

	for _, tc := range []struct{ cmd, state string }{
		{"deactivate", "disabled"},
		{"reactivate", "active"},
	} {
		err := userSetState([]string{"--login", "ghost", "--data-dir", dir}, tc.cmd, tc.state)
		if err == nil || !strings.Contains(err.Error(), "ghost") {
			t.Fatalf("user %s --login ghost: err = %v, want it to name the login", tc.cmd, err)
		}
	}

	if _, events := readState(t, dir, "alice"); len(events) != 0 {
		t.Errorf("a missing account still logged %+v", events)
	}
}
