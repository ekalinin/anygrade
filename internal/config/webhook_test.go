package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookCourse writes a minimal valid course whose only interesting part is the
// webhook block and returns the load + validate diagnostics. Going through
// LoadAll rather than building a Resolved by hand is deliberate: the new key
// has to survive the strict decoder too.
func hookCourse(t *testing.T, block string) []Diagnostic {
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
	write("course.yaml", "name: C\nregistration:\n  mode: invite\n"+block+
		"defaults:\n  runner:\n    type: local\n")
	write("tasks/one/main.go", "package main\n")
	write("tasks/one/task.yaml", "name: One\nscore: 100\nsolution_files: [main.go]\n"+
		"checks:\n  - name: plain\n    weight: 40\n    run: 'true'\n")

	r, diags, err := LoadAll(repo)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return append(diags, Validate(r)...)
}

// TestValidateWebhookTarget: the target is the one course.yaml key that makes
// the server connect out to a host of the teacher's choosing, so `validate`
// refuses what the deliverer would refuse - and refuses a URL with credentials
// in it, exactly like hidden_tests.url (SPEC §11).
func TestValidateWebhookTarget(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		error   string // expected error substring; "" = must validate clean
		warning string // expected warning substring
	}{
		{name: "absent"},
		{name: "https", url: "https://gradebook.example.edu/anygrade"},
		{name: "bad scheme", url: "ftp://example.edu/hook", error: "http or https"},
		{name: "no host", url: "https:///hook", error: "must include a host"},
		{name: "credentials", url: "https://bot:t0ken@example.edu/hook",
			error: "must not embed credentials"},
		{name: "plaintext", url: "http://gradebook.example.edu/hook",
			warning: "in the clear"},
		{name: "loopback", url: "http://127.0.0.1:9000/hook",
			warning: "loopback or private address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			block := ""
			if tc.url != "" {
				block = "webhook:\n  url: " + tc.url + "\n"
			}
			diags := hookCourse(t, block)

			var errs, warns []string
			for _, d := range diags {
				if !strings.HasPrefix(d.Field, "webhook") {
					continue
				}
				// The diagnostic is reported back in the teacher's push output,
				// and the target may carry a path that is itself a secret.
				if strings.Contains(d.Message, tc.url) && tc.url != "" {
					t.Errorf("diagnostic echoes the URL: %s", d)
				}
				if d.Severity == SevError {
					errs = append(errs, d.Message)
				} else {
					warns = append(warns, d.Message)
				}
			}
			joined := strings.Join(append(errs, warns...), "\n")

			if tc.error == "" && len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if tc.error != "" {
				if len(errs) == 0 {
					t.Fatalf("no error for %q, want %q", tc.url, tc.error)
				}
				if !strings.Contains(strings.Join(errs, "\n"), tc.error) {
					t.Fatalf("errors %v, want one mentioning %q", errs, tc.error)
				}
			}
			if tc.warning != "" && !strings.Contains(joined, tc.warning) {
				t.Fatalf("diagnostics %q, want one mentioning %q", joined, tc.warning)
			}
			if tc.error == "" && tc.warning == "" && len(diags) > 0 {
				t.Fatalf("expected a clean course, got: %v", diagStrings(diags))
			}
		})
	}
}

// TestWebhookURLIsResolved: the deliverer reads the target through the course
// holder, so it has to survive resolution rather than only decoding.
func TestWebhookURLIsResolved(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "course.yaml"),
		[]byte("name: C\nwebhook:\n  url: https://example.edu/hook\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _, err := LoadAll(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Course.Webhook.URL; got != "https://example.edu/hook" {
		t.Fatalf("resolved webhook.url = %q", got)
	}
}
