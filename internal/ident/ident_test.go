package ident

import "testing"

// TestValidLogin checks the shape rule shared by gitserver, web and cli, plus
// the one reserved name: "course" routes a post-receive hook to the upstream
// course repo rather than a student's own (SPEC §8), so it can never be a login.
func TestValidLogin(t *testing.T) {
	tests := []struct {
		login string
		want  bool
	}{
		{"alice", true},
		{"alice.bob", true},
		{"a1-b2_c3", true},
		{"", false},
		{"Alice", false},     // uppercase
		{"_template", false}, // must start alphanumeric
		{"a..b", false},      // no ".."
		{"course", false},    // reserved: the upstream-repo sentinel
	}
	for _, tc := range tests {
		if got := ValidLogin(tc.login); got != tc.want {
			t.Errorf("ValidLogin(%q) = %v, want %v", tc.login, got, tc.want)
		}
	}
}
