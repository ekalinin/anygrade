package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// leaderboardKeyFile holds the per-instance secret behind anonymized
// leaderboard aliases (SPEC §10). It lives in the data dir like every other
// mutable piece of state, so a backup and restore keeps the aliases stable.
const leaderboardKeyFile = "leaderboard.key"

// leaderboardSecretLen is a full HMAC-SHA256 block's worth of entropy.
const leaderboardSecretLen = 32

// loadLeaderboardSecret returns the instance's alias secret, generating and
// persisting one (0600) on first start. Aliases must survive restarts, so the
// secret cannot be per-process; it must not be derivable from anything public,
// so it cannot be the course name or the data dir path.
func loadLeaderboardSecret(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, leaderboardKeyFile)
	switch raw, err := os.ReadFile(path); {
	case err == nil:
		secret, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(secret) == 0 {
			return nil, errors.New(path + ": not a hex-encoded secret; remove it to regenerate " +
				"(leaderboard aliases will change)")
		}
		return secret, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	secret := make([]byte, leaderboardSecretLen)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	// O_EXCL: two processes opening the same data dir must not race into two
	// different secrets, which would silently reshuffle the board.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return loadLeaderboardSecret(dataDir)
		}
		return nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(hex.EncodeToString(secret) + "\n"); err != nil {
		return nil, err
	}
	return secret, nil
}
