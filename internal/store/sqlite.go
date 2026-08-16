package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; keeps the binary CGO-free
)

// DB is the SQLite-backed Store implementation.
type DB struct {
	db *sql.DB
}

var _ Store = (*DB)(nil)

// Open opens (creating if needed) the anygrade database in dataDir and applies
// pending migrations.
func Open(ctx context.Context, dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "anygrade.db")
	// _txlock=immediate: every transaction takes the write lock upfront,
	// avoiding lock-upgrade deadlocks. busy_timeout is a backstop only:
	// MaxOpenConns(1) below already serializes all access.
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(5000)"+
		"&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		url.PathEscape(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection makes SQLite's single-writer model structural:
	// concurrent workers' claims serialize instead of hitting SQLITE_BUSY.
	// INVARIANT: no transaction may be held across a check run (runner.Run),
	// or every other DB user in the process blocks on this one connection.
	// If M5 UI read load needs more, add a separate read-only handle; the
	// writer stays at 1.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db: db}, nil
}

// Close implements io.Closer.
func (s *DB) Close() error { return s.db.Close() }

// Timestamps are stored as RFC 3339 UTC strings of *fixed* width: lexicographic
// order equals chronological order, so string comparisons in SQL are correct.
// The width matters. time.RFC3339Nano drops trailing zeros of the fractional
// part, so ".387Z" and ".387026Z" are compared at their 4th fractional
// character - 'Z' (0x5A) against '0' (0x30) - and the earlier timestamp sorts
// last. Nine mandatory digits remove the case; migration 0004 rewrote the rows
// written before it.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

// parseTime stays on RFC3339Nano: it accepts any number of fractional digits,
// so it reads both timeLayout and anything a pre-0004 database still holds.
func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func parseTimePtr(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := parseTime(*s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
