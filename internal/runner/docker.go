package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekalinin/anygrade/internal/config"
)

// DockerRunner executes a submission's checks in one ephemeral container
// (`docker run --rm --detach` + `docker exec` per check). The workspace is
// never bind-mounted: /work is a tmpfs inside the container, seeded with
// `docker cp` (SPEC §14). Nothing a check writes reaches the host, and the
// container runs as a non-root user on every platform, because no host
// directory has to stay writable for it.
//
// Checks share the container, so filesystem state carries over from one check
// to the next, as it did with the old shared bind mount. A timed-out check
// takes the container down (killing the `docker exec` client would leave the
// process running inside it); the checks after it get a fresh container seeded
// from the workspace as it stood.
type DockerRunner struct {
	// User overrides the container --user value ("uid:gid"). Empty means the
	// default from userArg.
	User   string
	Mirror io.Writer // optional live copy of check output
}

const containerWorkdir = "/work"

// Run implements Runner.
func (r *DockerRunner) Run(ctx context.Context, job Job) ([]Outcome, error) {
	if err := r.ensureImage(ctx, job.Spec.Image); err != nil {
		return nil, err
	}
	s := &dockerSession{r: r, job: job}
	defer s.close(job.ExportWorkspace)
	return runAll(ctx, job, s)
}

// nobodyUID/nobodyGID are the fallback container credentials. 65534 is
// nobody:nogroup in every mainstream base image; the container never needs a
// resolvable account, only a non-zero id.
const nobodyUID, nobodyGID = 65534, 65534

// userArg resolves the container --user value and warns once when the root
// fallback kicks in - an operator has to be able to see it in the server log.
func (r *DockerRunner) userArg() string {
	u, fellBack := containerUser(r.User, os.Getuid(), os.Getgid())
	if fellBack {
		warnRootFallback(u)
	}
	return u
}

// containerUser picks the uid:gid the checks run as. An explicit User wins,
// but never root.
//
// The default is the uid:gid of the anygrade process on every platform: the
// workspace is seeded with `docker cp`, which keeps the uid of the host files,
// so the container user has to be the one that assembled the workspace.
// Nothing is bind-mounted any more, so there is no platform fork here (the old
// macOS exception existed only because colima's virtiofs remaps bind mounts to
// root:root).
//
// An anygrade running as root is the one case where that default is wrong:
// SPEC §14 requires student code to run non-root, and `--user 0:0` is exactly
// what it forbids. Falling back to a fixed unprivileged id is safer than
// refusing to run - refusing would turn every submission of a root deployment
// into a terminal error - and the seeded workspace is chowned to it (see
// dockerSession.ensure), so the checks can still write.
func containerUser(explicit string, uid, gid int) (user string, fellBack bool) {
	fallback := fmt.Sprintf("%d:%d", nobodyUID, nobodyGID)
	if explicit != "" {
		if u, _, ok := parseUserArg(explicit); ok && u == 0 {
			return fallback, true
		}
		return explicit, false
	}
	if uid == 0 || gid == 0 {
		return fallback, true
	}
	return fmt.Sprintf("%d:%d", uid, gid), false
}

// parseUserArg splits a numeric "uid:gid" (or bare "uid") value; ok=false for
// name-based values, which only the image can resolve.
func parseUserArg(user string) (uid, gid int, ok bool) {
	u, g, hasGID := strings.Cut(user, ":")
	uid, err := strconv.Atoi(u)
	if err != nil {
		return 0, 0, false
	}
	gid = uid
	if hasGID {
		if gid, err = strconv.Atoi(g); err != nil {
			return 0, 0, false
		}
	}
	return uid, gid, true
}

var rootFallbackOnce sync.Once

func warnRootFallback(user string) {
	rootFallbackOnce.Do(func() {
		slog.Warn("student containers must not run as root (SPEC §14): falling back to an unprivileged user",
			"user", user)
	})
}

// dockerSession owns the container of one submission. All checks are exec'd
// into it, so the tmpfs workspace they share behaves like the old bind mount:
// what one check writes, the next one sees.
type dockerSession struct {
	r    *DockerRunner
	job  Job
	name string // running container; "" before the first check and after a kill
}

// runArgs builds the `docker run` line of the submission container. The
// container only waits; checks are exec'd into it (see execCheck).
//
// /work is a tmpfs (SPEC §14) rather than a bind mount of the host workspace.
// It is a tmpfs-backed local volume and not a plain `--tmpfs`, because
// `docker cp` writes underneath a `--tmpfs` mount instead of into it - the
// copied files would be invisible to the container. Volume mounts are the one
// destination `docker cp` resolves through, and they are also exempt from the
// daemon's refusal to copy into a container with a read-only rootfs.
func (s *dockerSession) runArgs(name string) []string {
	job := s.job
	args := []string{
		"run", "--rm", "--detach", "--name", name,
		"--network", job.Spec.Network,
		"--memory", strconv.FormatInt(job.Spec.Memory, 10),
		"--memory-swap", strconv.FormatInt(job.Spec.Memory, 10), // swap == memory: no swap
		"--cpus", strconv.FormatFloat(job.Spec.CPUs, 'f', -1, 64),
		"--pids-limit", "512",
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,mode=1777,size=512m",
		// The inner quotes are required: docker parses --mount as CSV, and the
		// tmpfs option list carries commas of its own.
		"--mount", fmt.Sprintf("type=volume,dst=%s,volume-driver=local,volume-opt=type=tmpfs,"+
			"volume-opt=device=tmpfs,\"volume-opt=o=size=%d,nosuid,nodev\"", containerWorkdir, job.Spec.Memory),
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		// Redirect HOME and Go caches to the writable tmpfs so builds work
		// read-only and non-root regardless of the image's default user.
		"-e", "HOME=/tmp",
		"-e", "GOCACHE=/tmp/.cache/go-build",
		"-e", "GOMODCACHE=/tmp/go/pkg/mod",
		"-e", "GOPATH=/tmp/go",
	}
	if u := s.r.userArg(); u != "" {
		args = append(args, "--user", u)
	}
	// The container's init only waits. A check cannot take it down even though
	// it runs as the same user: the kernel delivers a signal to the init of a
	// PID namespace only if init installed a handler for it, and sleep has
	// none. `docker exec` inherits the user, env, limits and hardening above.
	return append(args, "--entrypoint", "sh", job.Spec.Image, "-c",
		fmt.Sprintf("sleep %d", int(s.lifetime().Seconds())))
}

// lifetime bounds how long the container waits for checks: enough for every
// check to hit its own timeout, plus slack for the docker round trips. Bounded
// on purpose - an anygrade that is killed mid-run must not leak a container
// (and its tmpfs) forever.
func (s *dockerSession) lifetime() time.Duration {
	return time.Duration(max(len(s.job.Checks), 1))*s.job.Spec.Timeout + 5*time.Minute
}

// ensureImage checks the image is present, pulling it if not. Front-loading
// the pull turns pull/daemon failures into a single InfraError before any
// check runs, and keeps the container start out of the first check's clock.
func (r *DockerRunner) ensureImage(ctx context.Context, image string) error {
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput()
	if err != nil {
		return infraErr("image_pull", fmt.Errorf("docker pull %s: %v: %s", image, err, lastLine(out)))
	}
	return nil
}

// ensure starts the submission container and seeds its tmpfs workspace. It is
// lazy so that a container killed after a timeout is simply rebuilt for the
// checks that follow.
func (s *dockerSession) ensure(ctx context.Context) error {
	if s.name != "" {
		return nil
	}
	name := "ag-" + randHex(10)
	out, err := exec.CommandContext(ctx, "docker", s.runArgs(name)...).CombinedOutput()
	if err != nil {
		return infraErr("container_create", fmt.Errorf("docker run failed: %s", lastLine(out)))
	}
	s.name = name
	// A root anygrade assembles a root-owned workspace, but the container runs
	// as the unprivileged fallback user (see containerUser), so hand the files
	// over first - `docker cp` keeps their uid.
	if os.Getuid() == 0 {
		if uid, gid, ok := parseUserArg(s.r.userArg()); ok {
			if err := chownTree(s.job.WorkspaceDir, uid, gid); err != nil {
				return infraErr("workspace", err)
			}
		}
	}
	// `docker cp` keeps the uid of the host files, which is why the container
	// runs as the process that assembled the workspace (see userArg).
	// "<dir>/." copies the contents of the workspace, not the directory itself.
	src := filepath.Clean(s.job.WorkspaceDir) + string(filepath.Separator) + "."
	out, err = exec.CommandContext(ctx, "docker", "cp", src, name+":"+containerWorkdir).CombinedOutput()
	if err != nil {
		return infraErr("workspace", fmt.Errorf("seed workspace: %v: %s", err, lastLine(out)))
	}
	return nil
}

// close removes the container, first copying /work back onto the host
// workspace when the caller asked for it. It runs on its own context so
// cleanup still happens after the parent was canceled.
func (s *dockerSession) close(export bool) {
	if s.name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if export {
		s.copyOut(ctx)
	}
	// --rm removes the container (and its anonymous tmpfs volume) on the happy
	// path; this covers kill/cancel races.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", s.name).Run()
	s.name = ""
}

// copyOut copies the container's /work onto the host workspace. Best effort:
// the workspace is a debugging aid (`--keep`) and a snapshot carried across a
// timeout, never a source of check results.
func (s *dockerSession) copyOut(ctx context.Context) {
	_ = exec.CommandContext(ctx, "docker", "cp",
		s.name+":"+containerWorkdir+"/.", s.job.WorkspaceDir).Run()
}

// alive reports whether the container is still running. Used to tell a failed
// check apart from a container that died under it (daemon restart, expired
// lifetime, OOM-killed init): the latter is an infrastructure error.
func (s *dockerSession) alive() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", s.name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func (s *dockerSession) execCheck(ctx context.Context, job Job, c config.Check, logPath string) (Outcome, error) {
	log, err := openCheckLog(logPath, c.Name, s.r.Mirror, job.Spec.LogExcerpt, job.Spec.LogMax)
	if err != nil {
		return Outcome{}, infraErr("workspace", err)
	}
	defer log.Close()

	if err := s.ensure(ctx); err != nil {
		return Outcome{}, err
	}

	cctx, cancel := context.WithTimeout(ctx, job.Spec.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "docker", "exec",
		"-w", path.Join(containerWorkdir, job.TaskRelDir), s.name, "sh", "-c", c.Run)
	cmd.Stdout = log
	cmd.Stderr = log

	start := time.Now()
	err = cmd.Run()
	dur := time.Since(start)

	timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
	if ctx.Err() != nil {
		return Outcome{}, infraErr("canceled", ctx.Err())
	}
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); !ok && !timedOut {
			return Outcome{}, infraErr("runner_exec", err)
		}
	}
	exit := cmd.ProcessState.ExitCode()
	switch {
	case timedOut:
		// Killing the `docker exec` client leaves the check running inside the
		// container, so the container itself has to go. Take the workspace
		// with it first: the checks that follow are seeded from it again.
		s.close(true)
		fmt.Fprintf(log, "\nanygrade: timed out after %s\n", job.Spec.Timeout)
	case exit != 0 && !s.alive():
		// The command never ran to completion - the container died under it.
		// Infrastructure, not a check failure: retried, no attempt consumed.
		s.close(false)
		return Outcome{}, infraErr("runner_exec",
			fmt.Errorf("container exited during check %q: %s", c.Name, lastLine([]byte(log.Excerpt()))))
	}

	return Outcome{
		Name:       c.Name,
		Passed:     !timedOut && exit == 0,
		ExitCode:   exit,
		Duration:   dur,
		TimedOut:   timedOut,
		LogPath:    logPath,
		LogExcerpt: log.Excerpt(),
	}, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func lastLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
