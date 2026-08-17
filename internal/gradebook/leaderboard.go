package gradebook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
)

// LeaderRow is one leaderboard entry (SPEC §10). Alias replaces Login for
// non-teachers when the course anonymizes the board.
type LeaderRow struct {
	Rank  int
	Login string
	Alias string
	Total float64
}

// Leaderboard ranks the matrix rows by total, descending, with standard
// competition ranking (1, 2, 2, 4).
func Leaderboard(m Matrix, a Aliaser) []LeaderRow {
	rows := make([]LeaderRow, 0, len(m.Rows))
	for _, r := range m.Rows {
		rows = append(rows, LeaderRow{Login: r.User.Login, Alias: a.Alias(r.User.Login), Total: r.Total})
	}
	slices.SortStableFunc(rows, func(a, b LeaderRow) int {
		switch {
		case a.Total > b.Total:
			return -1
		case a.Total < b.Total:
			return 1
		default:
			return 0
		}
	})
	for i := range rows {
		if i > 0 && rows[i].Total == rows[i-1].Total {
			rows[i].Rank = rows[i-1].Rank
		} else {
			rows[i].Rank = i + 1
		}
	}
	return rows
}

var aliasAdjectives = []string{
	"brave", "calm", "clever", "eager", "fuzzy", "gentle", "jolly", "keen",
	"lively", "mellow", "nimble", "proud", "quiet", "swift", "witty", "zesty",
}

var aliasAnimals = []string{
	"otter", "lynx", "heron", "badger", "falcon", "marten", "seal", "moose",
	"raven", "beaver", "stoat", "crane", "bison", "walrus", "puffin", "fox",
}

// Aliaser derives the anonymized leaderboard names of one instance. The secret
// is what makes them anonymous: the alias alphabet is small and the roster is
// guessable, so an unkeyed hash - which is what this used to be - de-anonymizes
// the whole board by offline brute force against the algorithm shipping in the
// binary. With a per-instance secret the same login still maps to the same
// alias for as long as that secret lives (SPEC §10: aliases must be stable),
// and nobody outside the server can reproduce the mapping.
type Aliaser struct {
	secret []byte
}

// NewAliaser binds an aliaser to an instance secret. The zero Aliaser produces
// guessable aliases and exists only so tests need not carry a secret; the
// composition root always passes one.
func NewAliaser(secret []byte) Aliaser { return Aliaser{secret: secret} }

// Alias derives a stable anonymized name from a login: no storage, no join
// order leak, deterministic across restarts of the same instance. The numeric
// suffix keeps large courses collision-free enough for a leaderboard.
func (a Aliaser) Alias(login string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(login))
	v := mac.Sum(nil)
	return fmt.Sprintf("%s-%s-%d",
		aliasAdjectives[v[0]%16], aliasAnimals[v[1]%16],
		binary.BigEndian.Uint16(v[2:4])%100)
}
