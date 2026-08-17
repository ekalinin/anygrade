package intake

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekalinin/anygrade/internal/hookproto"
)

// listen starts the intake listener on a socket in a fresh dir and returns its
// path once it accepts connections.
func listen(t *testing.T, s *Server) string {
	t.Helper()
	// The path has to stay well under the ~104 byte sun_path limit, which the
	// macOS temp dir alone almost fills.
	dir, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- s.ListenAndServe(ctx, socket) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	for range 100 {
		if _, err := os.Stat(socket); err == nil {
			return socket
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("intake socket never appeared")
	return ""
}

// TestSocketIsOwnerOnly: net.Listen leaves the socket world-connectable and the
// data dir around it is 0755, so any local account could otherwise speak the
// hook protocol and forge a push for any student (SPEC §14).
func TestSocketIsOwnerOnly(t *testing.T) {
	socket := listen(t, &Server{})
	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket mode = %v, want 0700", perm)
	}
	// The narrowing must not lock out the hook itself, which runs as us.
	resp, err := hookproto.Call(t.Context(), socket, hookproto.Request{Kind: "nope"})
	if err != nil {
		t.Fatalf("own-uid hook call: %v", err)
	}
	if !strings.Contains(strings.Join(resp.Lines, "\n"), "unknown hook kind") {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestOversizedRequestIsRefused: the request is a ref-update list, so an
// unbounded read is only ever a way to grow the server's heap from a socket.
func TestOversizedRequestIsRefused(t *testing.T) {
	socket := listen(t, &Server{})
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// A syntactically valid request whose actor field alone is 4 MB.
	req := hookproto.Request{Kind: hookproto.KindPreReceive, Actor: strings.Repeat("a", 4<<20)}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		// The server may already have hung up mid-write, which is the point.
		return
	}
	var resp hookproto.Response
	if err := json.NewDecoder(conn).Decode(&resp); err == nil {
		t.Fatalf("an oversized request was served: %+v", resp)
	}
}
