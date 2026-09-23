package store

import (
	"strings"
	"testing"
)

// TestCreateUserRejectsAnInvalidLogin: a login is also a path component - the
// student's repo is students/<login>.git and `export submissions` writes a
// directory per login next to its `_template/` - so the rule that keeps it one
// belongs at the write, not only in the four callers that happen to check it
// today.
func TestCreateUserRejectsAnInvalidLogin(t *testing.T) {
	db := openTestDB(t)
	for _, login := range []string{
		"", "Alice", "alice@uni.example", "../etc", "a..b", "_template",
		strings.Repeat("a", 65),
	} {
		if _, err := db.CreateUser(t.Context(), login, "A", RoleStudent); err == nil {
			t.Errorf("CreateUser(%q) was accepted", login)
		}
	}
	if _, err := db.CreateUser(t.Context(), "alice", "Alice", RoleStudent); err != nil {
		t.Errorf("CreateUser(alice): %v", err)
	}
}
