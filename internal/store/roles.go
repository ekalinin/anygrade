package store

// Roles a course account can hold (SPEC §8). The set is the users.role CHECK
// constraint's, so no other value ever reaches the predicates below.
const (
	RoleStudent = "student"
	RoleTA      = "ta"
	RoleTeacher = "teacher"
)

// ValidRole reports whether role is one the schema accepts; the CLI's --role
// asks before the INSERT so a typo is a message rather than a CHECK violation.
func ValidRole(role string) bool {
	return role == RoleStudent || role == RoleTA || role == RoleTeacher
}

// The two predicates below are the whole role table: callers ask what an
// account may DO, never what it IS, so a fourth role is a line here instead of
// a comparison in every handler. The line between them is what the TA role is
// for - reading and re-running the work without deciding the record.

// CanReview reports whether the account may look at other people's work: the
// matrix and its CSV export, every submission with its code and its full check
// logs (the build phase included), and the queue with cancel and recheck. It is
// also what "not a student" means on an anonymized leaderboard - anonymization
// exists so students cannot rank each other, not to keep staff from their job
// (SPEC §10).
func (u User) CanReview() bool {
	return u.Role == RoleTeacher || u.Role == RoleTA
}

// CanAdminister reports whether the account may change the record rather than
// read it: score overrides, token resets, SSH key deletion, deactivation, and
// the audit log that records all of it. Teachers only - a TA is trusted with
// the work, not with the accounts or the final score (SPEC §8).
func (u User) CanAdminister() bool {
	return u.Role == RoleTeacher
}
