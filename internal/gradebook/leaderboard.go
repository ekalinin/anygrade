package gradebook

import (
	"fmt"
	"hash/fnv"
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
func Leaderboard(m Matrix) []LeaderRow {
	rows := make([]LeaderRow, 0, len(m.Rows))
	for _, r := range m.Rows {
		rows = append(rows, LeaderRow{Login: r.User.Login, Alias: Alias(r.User.Login), Total: r.Total})
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

// Alias derives a stable anonymized name from a login: no storage, no join
// order leak, deterministic across restarts. The numeric suffix keeps large
// courses collision-free enough for a leaderboard.
func Alias(login string) string {
	h := fnv.New32a()
	h.Write([]byte(login))
	v := h.Sum32()
	return fmt.Sprintf("%s-%s-%d",
		aliasAdjectives[v%16], aliasAnimals[(v>>4)%16], (v>>8)%100)
}
