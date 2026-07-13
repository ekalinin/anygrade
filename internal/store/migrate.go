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
func migrate(ctx context.Context, db *sql.DB) error {
	var cur int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
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

		tx, err := db.BeginTx(ctx, nil)
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
