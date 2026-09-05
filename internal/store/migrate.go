package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies every migrations/*.sql file whose numeric prefix exceeds
// the database's current PRAGMA user_version, in ascending order.
//
// The ledger runs on one pinned connection with foreign keys off, which is
// what SQLite's own recipe for changing a table's schema requires: PRAGMA
// foreign_keys is per connection and a no-op inside a transaction, so it
// cannot be set by the migration that needs it (0010). Enforcement is not
// missed in the meantime - a migration rewrites whole tables rather than
// individual rows, and the pragma comes back with the connection, which is
// released before Open hands the store out.
func migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	// Restore the DSN's setting explicitly rather than trusting that this
	// connection is discarded: it is the only one the pool has.
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
	}()

	var cur int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if err := checkMigrationNumbers(names); err != nil {
		return err
	}

	for _, name := range names {
		n, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad numeric prefix: %w", name, err)
		}
		if n <= cur {
			continue
		}

		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("migration %s: read: %w", name, err)
		}

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migration %s: begin: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: exec: %w", name, err)
		}
		// PRAGMA cannot take ?-params; format the parsed int directly.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: set user_version: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: commit: %w", name, err)
		}
	}

	return nil
}

// checkMigrationNumbers refuses two migrations that claim the same number.
// user_version is a high-water mark, so a duplicate is not something the loop
// above would notice: whichever of the pair sorts first sets the version to
// their shared number, and the second is then skipped by `n <= cur` without a
// word. The database ends up missing one schema change and reporting itself up
// to date.
//
// Two branches numbering their migration in parallel is the ordinary way to
// arrive here, and by the time they meet both are already merged - so the
// check has to be the thing that notices. names must be sorted.
func checkMigrationNumbers(names []string) error {
	prev, prevName := -1, ""
	for _, name := range names {
		n, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad numeric prefix: %w", name, err)
		}
		if n == prev {
			return fmt.Errorf("migrations %s and %s share the number %d: "+
				"renumber the later one, or it is skipped silently", prevName, name, n)
		}
		prev, prevName = n, name
	}
	return nil
}
