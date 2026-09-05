// Package hookproto is the JSON protocol between the short-lived
// `anygrade hook` subprocess (spawned by git receive hooks) and the server's
// intake listener on a unix socket in the data dir. It must stay free of
// store/queue/config imports: both gitserver and intake depend on it.
package hookproto

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Hook kinds. A shim installed in a bare repo execs `anygrade hook <kind>`.
const (
	KindPreReceive     = "pre-receive"     // student repos: reserved-ref guard
	KindPostReceive    = "post-receive"    // student repos: submission intake; course.git: metadata reload
	KindValidateCourse = "validate-course" // course.git pre-receive: metadata validation
)

// Env variable names set by the server on the spawned receive-pack; they
// propagate into hooks (verified on git 2.54).
const (
	EnvSocket = "ANYGRADE_SOCKET" // unix socket path
	EnvRepo   = "ANYGRADE_REPO"   // repo owner login, or "course"
	EnvActor  = "ANYGRADE_ACTOR"  // authenticated login performing the push
	EnvRole   = "ANYGRADE_ROLE"   // student | ta | teacher
)

// RefUpdate is one `old new ref` line git feeds a receive hook on stdin.
// All-zero SHAs mean ref creation (Old) or deletion (New).
type RefUpdate struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// Request is the single message a hook sends per connection.
type Request struct {
	Kind    string      `json:"kind"`
	Repo    string      `json:"repo"`
	Actor   string      `json:"actor"`
	Role    string      `json:"role"`
	GitDir  string      `json:"git_dir"`
	Updates []RefUpdate `json:"updates"`

	// Quarantine environment (pre-receive/validate-course only): lets the
	// server read pushed-but-not-yet-accepted objects from its own processes.
	ObjectDir      string `json:"git_object_dir,omitempty"`
	AltObjectDirs  string `json:"git_alt_object_dirs,omitempty"`
	QuarantinePath string `json:"quarantine,omitempty"`
}

// Response tells the hook what to print (each line reaches the git client as
// a `remote:` sideband line) and how to exit. A non-zero exit from pre-receive
// rejects the push; post-receive exit codes cannot reject anything.
type Response struct {
	Lines    []string `json:"lines"`
	ExitCode int      `json:"exit_code"`
}

// Call performs one request/response exchange over the unix socket.
func Call(ctx context.Context, socket string, req Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return Response{}, fmt.Errorf("dial intake socket: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("send hook request: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("read hook response: %w", err)
	}
	return resp, nil
}
