package app

import (
	"strings"
	"testing"

	"github.com/ekalinin/anygrade/internal/config"
)

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:2222", true},
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"192.168.1.10:8080", false},
		{"example.com:8080", false},
	}
	for _, tc := range tests {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func resolvedWith(runnerTypes ...string) *config.Resolved {
	res := &config.Resolved{}
	for i, rt := range runnerTypes {
		res.Tasks = append(res.Tasks, config.ResolvedTask{
			ID:     "t" + string(rune('1'+i)),
			Runner: config.ResolvedRunner{Type: rt},
		})
	}
	return res
}

func TestCheckServeSafety(t *testing.T) {
	tests := []struct {
		name            string
		res             *config.Resolved
		httpAddr, ssh   string
		local, allow    bool
		wantErrContains string
	}{
		{
			name:     "loopback with local tasks is fine",
			res:      resolvedWith("local", "docker"),
			httpAddr: "127.0.0.1:8080", ssh: "localhost:2222",
		},
		{
			name:     "public bind, docker only, no flag needed",
			res:      resolvedWith("docker", "docker"),
			httpAddr: ":8080", ssh: ":2222",
		},
		{
			name:     "public bind with local task refuses",
			res:      resolvedWith("docker", "local"),
			httpAddr: ":8080", ssh: ":2222",
			wantErrContains: "--allow-local-runner",
		},
		{
			name:     "public ssh alone triggers the gate",
			res:      resolvedWith("local"),
			httpAddr: "127.0.0.1:8080", ssh: ":2222",
			wantErrContains: "--allow-local-runner",
		},
		{
			name:     "opt-in flag allows it",
			res:      resolvedWith("local"),
			httpAddr: ":8080", ssh: ":2222",
			allow: true,
		},
		{
			name:     "--local refuses public bind regardless of runners",
			res:      resolvedWith("docker"),
			httpAddr: "0.0.0.0:8080", ssh: "127.0.0.1:2222",
			local:           true,
			wantErrContains: "--local",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkServeSafety(tc.res, tc.httpAddr, tc.ssh, tc.local, tc.allow)
			if tc.wantErrContains == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Fatalf("error %v, want containing %q", err, tc.wantErrContains)
			}
		})
	}
}
