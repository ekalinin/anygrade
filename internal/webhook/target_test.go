package webhook

import (
	"strings"
	"testing"
)

// TestCheckURL pins the target policy `anygrade validate` and the deliverer
// share. A scheme outside the allowlist and userinfo are both errors: the first
// because nothing else is delivered over, the second because a credential in a
// URL inside the course repo is a credential every student has.
func TestCheckURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		bad  string // substring of the expected error; "" = must be accepted
	}{
		{"https", "https://gradebook.example.edu/anygrade", ""},
		{"https with a path and query", "https://example.edu/hook?course=go2026", ""},
		{"http", "http://example.edu/hook", ""},
		{"ssh", "ssh://git@example.edu/hook", "http or https"},
		{"file", "file:///tmp/hook", "http or https"},
		{"scheme-less", "example.edu/hook", "http or https"},
		{"no host", "https:///hook", "must include a host"},
		{"credentials", "https://bot:t0ken@example.edu/hook", "must not embed credentials"},
		{"bare username", "https://bot@example.edu/hook", "must not embed credentials"},
		{"garbage", "https://exa mple.edu/\x7f", "must be a valid URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckURL(tc.url)
			if tc.bad == "" {
				if err != nil {
					t.Fatalf("CheckURL(%q) = %v, want accepted", tc.url, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckURL(%q) accepted it, want %q", tc.url, tc.bad)
			}
			if !strings.Contains(err.Error(), tc.bad) {
				t.Fatalf("CheckURL(%q) = %v, want it to mention %q", tc.url, err, tc.bad)
			}
			// The diagnostic is echoed back in the teacher's push output, and
			// the target may carry a path that is itself a secret.
			if strings.Contains(err.Error(), tc.url) {
				t.Errorf("the error echoes the URL: %v", err)
			}
		})
	}
}

// TestPrivateHost covers the literal hosts validate warns about. A name is not
// resolved on purpose: the machine running validate is not the one that will
// dial, and the refusal is enforced at dial time regardless.
func TestPrivateHost(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://gradebook.example.edu/hook", false},
		{"https://93.184.216.34/hook", false},
		{"http://localhost:9000/hook", true},
		{"http://LocalHost:9000/hook", true},
		{"http://127.0.0.1:9000/hook", true},
		{"http://[::1]:9000/hook", true},
		{"http://10.0.0.5/hook", true},
		{"http://192.168.1.10/hook", true},
		{"http://172.16.0.1/hook", true},
		// The cloud metadata endpoint, which is the reason the policy exists.
		{"http://169.254.169.254/latest/meta-data/", true},
		{"http://[fd00::1]/hook", true},
		{"http://0.0.0.0/hook", true},
		// RFC 6598 shared address space, where a tailnet lives.
		{"http://100.64.0.1/hook", true},
		{"http://100.127.255.254/hook", true},
		{"http://100.63.0.1/hook", false}, // just below the range
		{"http://100.128.0.1/hook", false},
		// The same loopback address written as an IPv6 one.
		{"http://[::ffff:127.0.0.1]/hook", true},
		{"http://[::7f00:1]/hook", true},
		{"http://[64:ff9b::7f00:1]/hook", true},
		{"http://[64:ff9b::5db8:d822]/hook", false}, // NAT64 onto a public v4
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			if got := PrivateHost(tc.url); got != tc.want {
				t.Fatalf("PrivateHost(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
