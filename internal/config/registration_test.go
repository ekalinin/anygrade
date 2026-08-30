package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// regCourse writes a minimal valid course whose only interesting part is the
// registration block, loads it, and returns the load + validate diagnostics.
// Going through LoadAll rather than building a Resolved by hand is deliberate:
// the new keys have to survive the strict decoder too.
func regCourse(t *testing.T, registration string) (*Resolved, []Diagnostic) {
	t.Helper()
	repo := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("course.yaml", "name: C\nregistration:\n"+registration+"defaults:\n  runner:\n    type: local\n")
	write("tasks/one/main.go", "package main\n")
	write("tasks/one/task.yaml", "name: One\nscore: 100\nsolution_files: [main.go]\n"+
		"checks:\n  - name: plain\n    weight: 40\n    run: 'true'\n")

	r, diags, err := LoadAll(repo)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return r, append(diags, Validate(r)...)
}

func ts(t *testing.T, s string) *Timestamp {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	v := Timestamp(parsed)
	return &v
}

// TestRegistrationWindowOpenAt: the enrolment window is [opens, closes], the
// same closed interval a hard deadline uses, and an unset side is unbounded on
// that side (SPEC §8).
func TestRegistrationWindowOpenAt(t *testing.T) {
	at := func(s string) time.Time { return ts(t, s).Std() }
	opens := ts(t, "2026-09-01T00:00:00+03:00")
	closes := ts(t, "2026-09-15T00:00:00+03:00")

	tests := []struct {
		name string
		reg  Registration
		now  time.Time
		want bool
	}{
		{"unset is unbounded", Registration{}, at("2000-01-01T00:00:00Z"), true},
		{"before opens", Registration{Opens: opens, Closes: closes}, at("2026-08-31T23:59:59+03:00"), false},
		{"exactly at opens", Registration{Opens: opens, Closes: closes}, at("2026-09-01T00:00:00+03:00"), true},
		{"inside", Registration{Opens: opens, Closes: closes}, at("2026-09-07T12:00:00+03:00"), true},
		{"exactly at closes", Registration{Opens: opens, Closes: closes}, at("2026-09-15T00:00:00+03:00"), true},
		{"after closes", Registration{Opens: opens, Closes: closes}, at("2026-09-15T00:00:01+03:00"), false},
		{"opens only, before", Registration{Opens: opens}, at("2026-01-01T00:00:00Z"), false},
		{"opens only, after", Registration{Opens: opens}, at("2030-01-01T00:00:00Z"), true},
		{"closes only, before", Registration{Closes: closes}, at("2000-01-01T00:00:00Z"), true},
		{"closes only, after", Registration{Closes: closes}, at("2030-01-01T00:00:00Z"), false},
		// The offset is part of the value: the same instant written in another
		// zone must decide identically.
		{"offset is respected", Registration{Closes: closes}, at("2026-09-14T22:00:00+01:00"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.reg.OpenAt(tc.now); got != tc.want {
				t.Errorf("OpenAt(%s) = %v, want %v", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// TestLoadRegistrationBounds: the new keys decode, keep their offset, and
// reach ResolvedCourse.
func TestLoadRegistrationBounds(t *testing.T) {
	r, diags := regCourse(t, "  mode: open\n  course_code: c\n"+
		"  opens: 2026-09-01T00:00:00+03:00\n  closes: 2026-09-15T00:00:00+03:00\n  max_accounts: 40\n")
	if HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diagStrings(diags))
	}
	reg := r.Course.Registration
	if reg.Opens == nil || !reg.Opens.Std().Equal(ts(t, "2026-09-01T00:00:00+03:00").Std()) {
		t.Errorf("opens = %v", reg.Opens)
	}
	if reg.Closes == nil || !reg.Closes.Std().Equal(ts(t, "2026-09-15T00:00:00+03:00").Std()) {
		t.Errorf("closes = %v", reg.Closes)
	}
	if reg.MaxAccounts != 40 {
		t.Errorf("max_accounts = %d, want 40", reg.MaxAccounts)
	}
}

// TestLoadRegistrationBoundsDefaultUnbounded: a course.yaml written before
// these keys existed must keep meaning "open, forever, to anybody with the
// code" - the whole feature is additive (SPEC §8).
func TestLoadRegistrationBoundsDefaultUnbounded(t *testing.T) {
	r, diags := regCourse(t, "  mode: open\n  course_code: c\n")
	if HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diagStrings(diags))
	}
	reg := r.Course.Registration
	if reg.Opens != nil || reg.Closes != nil || reg.MaxAccounts != 0 {
		t.Fatalf("unset registration bounds must stay zero, got %+v", reg)
	}
	if !reg.OpenAt(time.Now()) {
		t.Error("an unbounded course must be open now")
	}
	if !reg.OpenAt(time.Unix(0, 0)) {
		t.Error("an unbounded course must be open at any time")
	}
}

func TestValidateRegistrationBounds(t *testing.T) {
	tests := []struct {
		name     string
		reg      string
		wantErr  string // "" = must validate clean
		wantWarn string
	}{
		{
			name:    "closes before opens",
			reg:     "  mode: open\n  course_code: c\n  opens: 2026-09-15T00:00:00Z\n  closes: 2026-09-01T00:00:00Z\n",
			wantErr: "registration.closes must be after registration.opens",
		},
		{
			name:    "closes equal to opens",
			reg:     "  mode: open\n  course_code: c\n  opens: 2026-09-01T00:00:00Z\n  closes: 2026-09-01T00:00:00Z\n",
			wantErr: "registration.closes must be after registration.opens",
		},
		{
			name:    "negative cap",
			reg:     "  mode: open\n  course_code: c\n  max_accounts: -1\n",
			wantErr: "must be >= 0",
		},
		{
			name:    "offset-less timestamp",
			reg:     "  mode: open\n  course_code: c\n  closes: 2026-09-01\n",
			wantErr: "must be RFC3339 with an explicit offset",
		},
		{
			name:     "window in invite mode never applies",
			reg:      "  mode: invite\n  closes: 2026-09-01T00:00:00Z\n",
			wantWarn: "has no effect when registration.mode is invite",
		},
		{
			name:     "cap in invite mode never applies",
			reg:      "  mode: invite\n  max_accounts: 5\n",
			wantWarn: "has no effect when registration.mode is invite",
		},
		{
			name: "a bounded open course is valid",
			reg:  "  mode: open\n  course_code: c\n  opens: 2026-09-01T00:00:00+03:00\n  closes: 2026-09-15T00:00:00+03:00\n  max_accounts: 40\n",
		},
		{
			name: "an unlimited cap in invite mode says nothing",
			reg:  "  mode: invite\n  max_accounts: 0\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := regCourse(t, tc.reg)
			joined := strings.Join(diagStrings(diags), "\n")
			switch {
			case tc.wantErr != "":
				if !HasErrors(diags) {
					t.Fatalf("expected an error, got:\n%s", joined)
				}
				if !strings.Contains(joined, tc.wantErr) {
					t.Errorf("missing %q in:\n%s", tc.wantErr, joined)
				}
			case tc.wantWarn != "":
				if HasErrors(diags) {
					t.Fatalf("a setting that never applies must be a warning, not an error:\n%s", joined)
				}
				if !strings.Contains(joined, tc.wantWarn) {
					t.Errorf("missing warning %q in:\n%s", tc.wantWarn, joined)
				}
			default:
				if len(diags) != 0 {
					t.Errorf("expected no diagnostics, got:\n%s", joined)
				}
			}
		})
	}
}
