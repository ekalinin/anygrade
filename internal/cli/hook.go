package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ekalinin/anygrade/internal/hookproto"
)

// cmdHook implements the hidden `anygrade hook <kind>` plumbing command: it
// forwards a git receive hook invocation to the server over the unix socket
// (SPEC §6). kind is pre-receive | post-receive | validate-course.
func cmdHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: anygrade hook <pre-receive|post-receive|validate-course>")
		return 1
	}
	kind := args[0]
	switch kind {
	case hookproto.KindPreReceive, hookproto.KindPostReceive, hookproto.KindValidateCourse:
	default:
		fmt.Fprintf(os.Stderr, "anygrade hook: unknown kind %q\n", kind)
		return 1
	}

	socket := os.Getenv(hookproto.EnvSocket)
	if socket == "" {
		if kind == hookproto.KindPostReceive {
			fmt.Println("anygrade: server socket not configured; push stored, grading deferred")
			return 0
		}
		fmt.Fprintln(os.Stderr, "anygrade: server socket not configured")
		return 1
	}

	gitDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "anygrade: %v\n", err)
		return 1
	}

	req := hookproto.Request{
		Kind:           kind,
		Repo:           os.Getenv(hookproto.EnvRepo),
		Actor:          os.Getenv(hookproto.EnvActor),
		Role:           os.Getenv(hookproto.EnvRole),
		GitDir:         gitDir,
		Updates:        readRefUpdates(os.Stdin),
		ObjectDir:      os.Getenv("GIT_OBJECT_DIRECTORY"),
		AltObjectDirs:  os.Getenv("GIT_ALTERNATE_OBJECT_DIRECTORIES"),
		QuarantinePath: os.Getenv("GIT_QUARANTINE_PATH"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := hookproto.Call(ctx, socket, req)
	if err != nil {
		if kind == hookproto.KindPostReceive {
			fmt.Println("anygrade: server unavailable; push stored, grading deferred")
			return 0
		}
		fmt.Fprintf(os.Stderr, "anygrade: %v\n", err)
		return 1
	}

	for _, line := range resp.Lines {
		fmt.Println(line)
	}
	return resp.ExitCode
}

// readRefUpdates parses `old new ref` lines git feeds a receive hook on
// stdin, skipping malformed lines. Read errors just end the list: the server
// treats an empty update set as a no-op push.
func readRefUpdates(r io.Reader) []hookproto.RefUpdate {
	var updates []hookproto.RefUpdate
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}
		updates = append(updates, hookproto.RefUpdate{Old: fields[0], New: fields[1], Ref: fields[2]})
	}
	_ = scanner.Err()
	return updates
}
