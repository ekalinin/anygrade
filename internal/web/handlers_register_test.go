package web

import "testing"

func TestGitURLs(t *testing.T) {
	h := &Handler{BaseURL: "http://grade.example.org:8080/", SSHAddr: ":2222"}
	u := h.gitURLs("alice")
	if u.Clone != "http://grade.example.org:8080/git/alice/course.git" {
		t.Errorf("clone: %s", u.Clone)
	}
	if u.Upstream != "http://grade.example.org:8080/git/course.git" {
		t.Errorf("upstream: %s", u.Upstream)
	}
	if u.SSHClone != "ssh://git@grade.example.org:2222/alice/course.git" {
		t.Errorf("ssh clone: %s", u.SSHClone)
	}
	if u.SSHUpstream != "ssh://git@grade.example.org:2222/course.git" {
		t.Errorf("ssh upstream: %s", u.SSHUpstream)
	}
}

func TestGitURLsNoSSH(t *testing.T) {
	h := &Handler{BaseURL: "http://localhost:8080"}
	if u := h.gitURLs("alice"); u.SSHClone != "" || u.SSHUpstream != "" {
		t.Errorf("ssh urls must be empty without an ssh addr: %+v", u)
	}
}

// TestGitURLsSSHHostFallback: an unparseable base URL still yields a usable
// localhost SSH hint rather than none.
func TestGitURLsSSHHostFallback(t *testing.T) {
	h := &Handler{BaseURL: "", SSHAddr: "0.0.0.0:2222"}
	if got := h.gitURLs("bob").SSHClone; got != "ssh://git@localhost:2222/bob/course.git" {
		t.Errorf("ssh clone: %s", got)
	}
}
