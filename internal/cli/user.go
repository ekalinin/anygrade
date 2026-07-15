package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
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
	if _, err := db.AddSSHKey(ctx, u.ID, fingerprint, strings.TrimSpace(*key)); err != nil {
		return err
	}

	fmt.Printf("key %s added for %s\n", fingerprint, u.Login)
	return nil
}

func userInvite(args []string) error {
	fs := flag.NewFlagSet("user invite", flag.ContinueOnError)
	login := fs.String("login", "", "user login")
	name := fs.String("name", "", "display name")
	role := fs.String("role", "student", "role: student|teacher")
	expires := fs.Duration("expires", 336*time.Hour, "invite validity duration")
	baseURL := fs.String("base-url", "http://localhost:8080", "base URL for the invite link")
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
		// A re-invite of an existing user is fine; any other error is fatal.
		if !strings.Contains(err.Error(), "UNIQUE constraint") {
			return err
		}
		u, err = db.GetUserByLogin(ctx, *login)
		if err != nil {
			return err
		}
	}

	plaintext, err := newInviteToken()
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(*expires)
	if err := db.CreateInvite(ctx, u.ID, plaintext, expiresAt); err != nil {
		return err
	}

	fmt.Printf("invite for %s: %s/invite/%s\n", u.Login, *baseURL, plaintext)
	fmt.Printf("expires: %s\n", expiresAt.Format(time.RFC3339))
	fmt.Println("the link is one-shot; it lets the student set up a token and SSH key")
	return nil
}

// newInviteToken returns a fresh "inv_"-prefixed random token.
func newInviteToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "inv_" + hex.EncodeToString(buf), nil
}
