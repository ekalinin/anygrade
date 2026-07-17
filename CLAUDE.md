# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Working agreements

- Fable (or Opus, if Fable is not available) is the main one, and it shouldn't have to do all the work itself; reasoning-heavy tasks are sent to the `deep-reasoner`, and mechanics are sent to the `fast-worker`
- /use-modern-go for go-code (the module targets Go 1.26: `errors.AsType`, `wg.Go`, `t.Context()`, `for range n` are the norm here)
- use colima to run docker under macos
- add all repetitive commands (tests, linters, generation, etc.) to the Makefile
- the user commits himself; prepare a commit message instead of committing

## Commands

- `make check` - build + vet + gofmt + full unit tests; run before handing work off
- `make e2e` - end-to-end regression suite (spawns the real binary, drives CLI/HTTP/git; needs `git`, no docker, ~6s)
- `make test-short` - unit tests with `-short`
- `make binary` - build `./anygrade`
- single test: `go test ./internal/<pkg> -run TestName -count=1`
- e2e files are behind a build tag: `go test -tags e2e ./e2e` (plain `./...` skips them; gopls shows "no packages found" for them - not an error)
- `./anygrade validate --repo <dir>` - check course metadata

## Architecture

One binary turns a git repo of course tasks into a grading system. SPEC.md is the authoritative spec (section references like §7 point there); README.md covers user-facing behavior. `example/go-web-dev-2024-04-26/` is a real configured course.

Submission flow (the path that touches most packages):

1. A student pushes to their personal bare repo - SSH or smart HTTP, both wrap the system `git` binary (`upload-pack`/`receive-pack`); no go-git (`internal/gitserver`).
2. The receive hook is the binary re-executing itself (`anygrade hook`); it talks to the server over a unix socket in the data dir (`internal/hookproto`), so hooks work from any bare repo cwd.
3. `internal/intake` diffs the pushed head against the last processed baseline ref, maps changed paths to tasks, and admits or rejects per task (deadlines, attempt limits). Pushes are never rejected for policy reasons - the submission is, in the push output. A first push has no baseline and diffs against the empty tree, so every task is detected.
4. `internal/queue` stores submissions in SQLite; a worker pool claims them. Running submissions are re-queued on restart. `queue.Terminal(msg)` marks a prepare failure non-retryable.
5. Prep assembles an ephemeral workspace: authoritative task files from the course mirror + only the student's `solution_files` from their commit + hidden tests. Student edits to task.yaml/open tests are discarded and noted for the teacher.
6. `internal/runner` executes each check via `sh -c` (local runner) or in an ephemeral docker container; `internal/scoring` and `internal/gradebook` turn results into scores.
7. `internal/web` (SSR html/template + htmx + SSE, all embedded) streams live status via the Hub.

Package boundaries to preserve:

- `internal/app` is the composition root - the only place where store, queue, gitserver, and intake concrete types are wired together. `web` never imports `gitserver` (git reads are injected as closures); `gitserver` is store-free (auth behind a small interface).
- `internal/store`: SQLite with `MaxOpenConns(1)` + WAL - CLI and server safely share one DB from separate processes. Never hold a transaction across `runner.Run`.
- `internal/config` merges course.yaml defaults into per-task `Resolved*` types; `intake.Holder` swaps the active course atomically when the teacher pushes a metadata update (invalid metadata rejects the teacher's push with the error list).
- `internal/hidden`: cache of hidden-test repos (one bare mirror per URL, TTL coalescing, offline fallback to a pinned ref). Credentials come only from the environment (`ANYGRADE_HIDDEN_GIT_TOKEN`), never from the course repo, and never appear in argv. Every student-visible error is scrubbed to "hidden tests temporarily unavailable"; full detail goes to the server log only. `anygrade check` never fetches hidden tests.
- `internal/ratelimit`: one failure-only limiter instance shared between the web login form and git HTTP basic auth, keyed by (client IP, login).

Auth model: personal tokens `ag_` + 64 hex, shown once and stored hashed - the same token is the web login credential and the git HTTP basic-auth password. SSH keys authenticate only the SSH transport (lookup by SHA256 fingerprint, username ignored). Students see 404, not 403, on teacher routes.

Everything mutable lives in the data dir (default `<repo>/.anygrade`): SQLite DB, bare course mirror + per-student repos, hidden-tests cache, check logs, workspaces.
