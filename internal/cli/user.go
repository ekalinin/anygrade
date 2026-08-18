package cli

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ekalinin/anygrade/internal/ident"
	"github.com/ekalinin/anygrade/internal/store"
)

// cmdUser implements `anygrade user <subcommand>` (SPEC §8).
func cmdUser(args []string) int {
	if len(args) == 0 {
		printUserUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	var err error
	switch sub {
	case "add":
		err = userAdd(rest)
	case "list":
		err = userList(rest)
	case "remove":
		err = userRemove(rest)
	case "reset-token":
		err = userResetToken(rest)
	case "add-key":
		err = userAddKey(rest)
	case "invite":
		err = userInvite(rest)
	default:
		printUserUsage()
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade user: %v\n", err)
		return 1
	}
	return 0
}

func printUserUsage() {
	fmt.Fprintln(os.Stderr, "usage: anygrade user <add|list|remove|reset-token|add-key|invite> [flags]")
}

func userAdd(args []string) error {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	login := fs.String("login", "", "user login")
	name := fs.String("name", "", "display name")
	role := fs.String("role", "student", "role: student|teacher")
	dataDir := fs.String("data-dir", ".anygrade", "anygrade data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *login == "" {
		return fmt.Errorf("--login is required")
	}
	if !ident.ValidLogin(*login) {
		return fmt.Errorf("invalid login %q (lowercase letters, digits, ._-)", *login)
	}
	if *role != "student" && *role != "teacher" {
		return fmt.Errorf("--role must be student or teacher, got %q", *role)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	u, err := db.CreateUser(ctx, *login, *name, *role)
	if err != nil {
		return err
	}
	token, err := db.IssueToken(ctx, u.ID)
	if err != nil {
		return err
	}

	fmt.Printf("user %s created (role %s)\n", u.Login, u.Role)
	fmt.Printf("token: %s\n", token)
	fmt.Println("store this token now; it is shown only once")
	return nil
}

func userList(args []string) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	dataDir := fs.String("data-dir", ".anygrade", "anygrade data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	users, err := db.ListUsers(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LOGIN\tNAME\tROLE\tSTATE\tCREATED")
	for _, u := range users {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", u.Login, u.DisplayName, u.Role, u.State, u.CreatedAt.Format("2006-01-02"))
	}
	return w.Flush()
}

func userRemove(args []string) error {
	fs := flag.NewFlagSet("user remove", flag.ContinueOnError)
	login := fs.String("login", "", "user login")
	dataDir := fs.String("data-dir", ".anygrade", "anygrade data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *login == "" {
		return fmt.Errorf("--login is required")
	}

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.SetUserState(ctx, *login, "disabled"); err != nil {
		return err
	}
	fmt.Printf("user %s deactivated\n", *login)
	return nil
}

func userResetToken(args []string) error {
	fs := flag.NewFlagSet("user reset-token", flag.ContinueOnError)
	login := fs.String("login", "", "user login")
	dataDir := fs.String("data-dir", ".anygrade", "anygrade data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *login == "" {
		return fmt.Errorf("--login is required")
	}

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	u, err := db.GetUserByLogin(ctx, *login)
	if err != nil {
		return err
	}
	token, err := db.IssueToken(ctx, u.ID)
	if err != nil {
		return err
	}

	fmt.Printf("token reset for %s\n", u.Login)
	fmt.Printf("token: %s\n", token)
	fmt.Println("store this token now; it is shown only once")
	return nil
}

// userAddKey is the teacher's out-of-band path for a student who cannot use
// the settings page, and the one registration that carries no proof of
// possession (SPEC §8).
//
// It stays unproven on purpose. Requiring a signature here would protect
// nothing: whoever runs this command already holds the data dir, and can
// `user add` an account or `reset-token` their way into any existing one, so
// the trust in the key is the teacher's trust in however they received it. The
// threat proof of possession answers is student against student, and students
// do not reach the CLI. What the command must not become is a quiet bypass, so
// the key is recorded as unproven - visible as such on the student's page, and
// losable to whoever later proves possession of that fingerprint.
func userAddKey(args []string) error {
	fs := flag.NewFlagSet("user add-key", flag.ContinueOnError)
	login := fs.String("login", "", "user login")
	key := fs.String("key", "", "public key, authorized_keys format")
	dataDir := fs.String("data-dir", ".anygrade", "anygrade data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *login == "" {
		return fmt.Errorf("--login is required")
	}
	if *key == "" {
		return fmt.Errorf("--key is required")
	}

	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(*key))
	if err != nil {
		return fmt.Errorf("parse ssh key: %w", err)
	}
	fingerprint := ssh.FingerprintSHA256(pk)

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	u, err := db.GetUserByLogin(ctx, *login)
	if err != nil {
		return err
	}
	// Name the current holder instead of surfacing a UNIQUE constraint: a
	// contested fingerprint is exactly the case a teacher has to resolve, and
	// the raw SQLite text says nothing about who to talk to.
	if holder, held, herr := db.KeyHolder(ctx, fingerprint); herr == nil && held {
		return fmt.Errorf("fingerprint %s is already registered to %s; remove it from that student's page first",
			fingerprint, holder.Login)
	}
	// Store the re-marshalled key, not the pasted text: ParseAuthorizedKey reads
	// the first line and ignores the rest, and it accepts authorized_keys
	// options in front of the key, so the argument may hold far more than the
	// key its fingerprint was taken from.
	if _, err := db.AddSSHKey(ctx, u.ID, fingerprint,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))); err != nil {
		return err
	}

	fmt.Printf("key %s added for %s\n", fingerprint, u.Login)
	fmt.Println("recorded without proof of possession: it is shown as unproven,")
	fmt.Println("and whoever proves possession of this fingerprint can take it over")
	return nil
}

func userInvite(args []string) error {
	fs := flag.NewFlagSet("user invite", flag.ContinueOnError)
	login := fs.String("login", "", "user login")
	name := fs.String("name", "", "display name")
	csvPath := fs.String("csv", "", "CSV roster file (login[,display_name] per row) instead of --login/--name")
	role := fs.String("role", "student", "role: student|teacher")
	expires := fs.Duration("expires", 336*time.Hour, "invite validity duration")
	baseURL := fs.String("base-url", "http://localhost:8080", "base URL for the invite link")
	dataDir := fs.String("data-dir", ".anygrade", "anygrade data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *role != "student" && *role != "teacher" {
		return fmt.Errorf("--role must be student or teacher, got %q", *role)
	}

	var roster []rosterEntry
	if *csvPath != "" {
		if *login != "" || *name != "" {
			return fmt.Errorf("--csv cannot be combined with --login or --name")
		}
		f, err := os.Open(*csvPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if roster, err = parseRoster(f); err != nil {
			return err
		}
	} else {
		if *login == "" {
			return fmt.Errorf("--login is required")
		}
		if !ident.ValidLogin(*login) {
			return fmt.Errorf("invalid login %q (lowercase letters, digits, ._-)", *login)
		}
		roster = []rosterEntry{{Login: *login, Name: *name}}
	}

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	expiresAt := time.Now().Add(*expires)
	for _, e := range roster {
		if err := inviteOne(ctx, db, e.Login, e.Name, *role, expiresAt, *baseURL); err != nil {
			return err
		}
	}

	fmt.Printf("expires: %s\n", expiresAt.Format(time.RFC3339))
	fmt.Println("the link is one-shot; it lets the student set up a token")
	fmt.Println("SSH keys are added afterwards in settings, against a signed challenge")
	return nil
}

// inviteOne creates (or re-invites) a single user and prints its activation
// link; shared by the single-user and CSV roster paths of `user invite`.
func inviteOne(ctx context.Context, db store.Store, login, name, role string, expiresAt time.Time, baseURL string) error {
	u, err := db.CreateUser(ctx, login, name, role)
	if err != nil {
		// A re-invite of an existing user is fine; any other error is fatal.
		if !strings.Contains(err.Error(), "UNIQUE constraint") {
			return err
		}
		u, err = db.GetUserByLogin(ctx, login)
		if err != nil {
			return err
		}
	}

	plaintext, err := newInviteToken()
	if err != nil {
		return err
	}
	if err := db.CreateInvite(ctx, u.ID, plaintext, expiresAt); err != nil {
		return err
	}

	fmt.Printf("invite for %s: %s/invite/%s\n", u.Login, baseURL, plaintext)
	return nil
}

// rosterEntry is one validated CSV roster row.
type rosterEntry struct {
	Login string
	Name  string
}

// parseRoster reads a CSV roster (login[,display_name] per row; extra
// columns are ignored). A leading "login" header row is detected and
// skipped. Every row is validated before any is returned, so a bad row
// (reported with its 1-based line number, counting the header) fails the
// whole import cleanly.
func parseRoster(r io.Reader) ([]rosterEntry, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}

	rowOffset := 1
	if len(records) > 0 && len(records[0]) > 0 && strings.TrimSpace(records[0][0]) == "login" {
		records = records[1:]
		rowOffset = 2
	}

	roster := make([]rosterEntry, 0, len(records))
	for i, rec := range records {
		var login, name string
		if len(rec) > 0 {
			login = strings.TrimSpace(rec[0])
		}
		if len(rec) > 1 {
			name = strings.TrimSpace(rec[1])
		}
		if login == "" || !ident.ValidLogin(login) {
			return nil, fmt.Errorf("row %d: invalid login %q", i+rowOffset, login)
		}
		roster = append(roster, rosterEntry{Login: login, Name: name})
	}
	return roster, nil
}

// newInviteToken returns a fresh "inv_"-prefixed random token.
func newInviteToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "inv_" + hex.EncodeToString(buf), nil
}
