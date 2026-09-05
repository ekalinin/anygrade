# anygrade

A single Go binary that turns any git repository with course tasks into a grading system.

Point it at a course repo and it serves:

- a git interface for submitting solutions (SSH and smart HTTP),
- a web UI for students (results, live logs, scores) and teachers (matrix, overrides, CSV export, queue, audit), with a TA role for assistants who review work but do not manage accounts, in English or Russian (`language:` in course.yaml, plus a per-user switcher),
- a local mode for offline self-checking and course authoring.

Behavior is driven by metadata files (`course.yaml`, `task.yaml`), not by code changes. A check is an arbitrary command, so any language works if the environment (docker image or host) can run it. One running instance serves exactly one course.

## How it works

1. The teacher keeps tasks in a plain git repo; each task is a directory with a `task.yaml`.
2. Each student gets a personal server-side clone. They clone it, edit, commit, push.
3. A push hook diffs the branch head against the last processed commit, maps changed paths to tasks, and queues one submission per changed task. Feedback starts right in the push output:

   ```
   remote: anygrade: 2 task(s) detected
   remote:   01-intro   submission #142 queued   http://host/submissions/142
   remote:   02-structs rejected: hard deadline passed (2026-10-01 23:59 +03)
   ```

4. A worker builds a clean workspace (authoritative task files + the student's `solution_files` + hidden tests), runs the checks in docker or on the host, and stores results. The student watches progress live in the web UI.

Editing open tests, `task.yaml`, or build files in the student repo is useless: the authoritative versions are restored before checking, and such modifications are noted for the teacher. Pushes are never rejected for policy reasons - deadlines and attempt limits reject the submission, not the push. A pack larger than `limits.max_push_size` is the one thing the server does stop, and it says why:

```
remote: anygrade: push rejected: it is larger than max_push_size (50 MB); drop the large files from the commit (git rm --cached, then amend) and push again
```

## Course repo layout

```
course-repo/
  course.yaml          # course-level config and defaults
  tasks/
    01-intro/
      task.yaml        # checks, score, deadline, limits
      README.md        # task statement
      main.go          # solution template
      main_test.go     # open tests, visible to students
```

`course.yaml`:

```yaml
name: "Go course 2026"
language: en              # web UI language: en | ru (default en)
timezone: Europe/Berlin   # IANA name the UI renders times in (default UTC)

registration:
  mode: invite            # invite | open
  # course_code: "go-2026"  # required when mode: open
  # opens: 2026-09-01T00:00:00+03:00   # optional enrolment window (open mode),
  # closes: 2026-09-15T23:59:59+03:00  # inclusive on both ends
  # max_accounts: 40        # optional cap on self-registered accounts (0 = unlimited)

scoring:
  policy: best            # best | latest - which submission counts per task

leaderboard:
  enabled: true
  anonymize: false        # true shows stable aliases instead of logins

limits:                   # course-wide, unrelated to the per-task defaults below
  max_push_size: 50m      # largest pack one student push may carry (default 50m)

defaults:                 # inherited by every task.yaml, overridable per task
  runner:
    type: docker          # docker | local
    image: golang:1.24
    timeout: 5m
    memory: 512m
    cpus: 1
    network: none
    log_excerpt: 64k      # per-check log tail kept in the DB/UI (default 64k)
    log_max: 10m          # per-check log kept on disk, then truncated (10m)
  workspace:
    max_file_size: 10m    # per solution file copied out of the student commit
    max_total_size: 64m   # all solution files of one submission together (64m)
```

`task.yaml`:

```yaml
name: "Intro task"
score: 100

solution_files:           # allowlist of student-editable paths
  - main.go

deadline:
  soft: 2026-09-24T23:59:59+03:00
  hard: 2026-10-01T23:59:59+03:00
  penalty: {percent: 10, per: 24h, max_percent: 50}

limits:
  max_attempts: 20
  cooldown: 5m

checks:
  - name: build
    required: true        # gate: failure stops the run, submission scores 0
    weight: 0
    run: go build ./...
  - name: basic
    weight: 60
    run: go test -run 'TestBasic' ./...
  - name: advanced
    weight: 40
    run: go test -run 'TestAdvanced' ./...
```

Raw score = `score × (passed weight / total weight)`. Weights must be non-negative - a negative one would push the score past the maximum, so `anygrade validate` rejects it; weight 0 is what gates carry. Late submissions between the soft and hard deadline get a percentage penalty per started interval; past the hard deadline they are recorded but not graded.

A check may also carry an optional `build:` phase before its `run:`; it passes iff both exit 0. That is what keeps a compiled language's hidden tests off the disk while the student's code runs - see [hidden tests](#hidden-tests).

Check output is kept twice: the last `log_excerpt` bytes go to the database and the UI, the whole stream to a file capped at `log_max` and closed with a truncation marker. Students see the excerpt and the live stream; the full log is staff-only (teachers and TAs), to read in the browser or to download.

## Install

Grab the archive for your platform from the [releases page](https://github.com/ekalinin/anygrade/releases), unpack it and put the binary on your `PATH` (`checksums.txt` holds the SHA256 of every archive):

```sh
tar xzf anygrade_<version>_linux_amd64.tar.gz
sudo install anygrade /usr/local/bin/
anygrade version
```

Or build it yourself, which also works for the latest untagged code:

```sh
go install github.com/ekalinin/anygrade/cmd/anygrade@latest
```

Releases cover linux and darwin, amd64 and arm64. There is no windows build: the server needs `sh`, process groups and a unix socket.

## Quick start

```sh
go build -o anygrade ./cmd/anygrade

# in the course repo: validate metadata and create the first teacher
./anygrade validate
./anygrade user add --login prof --role teacher

# serve (add .anygrade/ to the course repo's .gitignore)
./anygrade serve --http-addr :8080 --ssh-addr :2222
```

Students are registered by invite links (`anygrade user invite --login alice`, or `--csv roster.csv` for a whole group) or self-register with a course code when `registration.mode: open`. The course code is a shared secret that lives in the repo every student clones, so open mode takes two optional bounds: an enrolment window (`registration.opens` / `registration.closes`) and a cap on how many accounts self-registration may create (`registration.max_accounts`, counting only accounts created by the form - invited ones are free). Both are unset by default, which is the historical unbounded behavior. The activation page issues a personal token and prints the git setup. Two transports are available:

```sh
# over HTTP: username = login, password = the token
git clone http://host:8080/git/<login>/course.git
git remote add upstream http://host:8080/git/course.git

# or over SSH, once an SSH key is added on the settings page
git clone ssh://git@host:2222/<login>/course.git
git remote add upstream ssh://git@host:2222/course.git

# later: git pull upstream main
```

The token is the basic-auth password for git over HTTP and the login credential for the web UI. SSH auth is by key only; the token is not asked for.

Adding an SSH key takes two steps, because public keys are public: paste the key on the settings page, and the server hands back a one-time challenge to sign with the private half.

```sh
printf '%s' 'anygrade-key-proof/v1 user=alice key=SHA256:... nonce=agc_...' \
  | ssh-keygen -Y sign -f ~/.ssh/id_ed25519 -n anygrade -
```

The page prints the exact line; it names your login and the key, so a line somebody else sends you would register your key to their account. Paste the whole `-----BEGIN SSH SIGNATURE-----` block back and the key is registered. The challenge lasts ten minutes and works once. Keys added by a teacher with `anygrade user add-key`, and keys registered by older versions, carry no such proof: they keep working, are labelled unproven on the settings and student pages, and lose the fingerprint to whoever later proves possession of it.

Teachers push course updates to `/git/course.git` - every push is validated and rejected with the error list if the metadata is broken.

### Roles

`--role` takes `student`, `ta` or `teacher`, on `user add` and on `user invite` alike. A TA is a course assistant with the reviewing half of a teacher's rights and none of the account-management half:

| | student | ta | teacher |
|---|---|---|---|
| own dashboard, tasks, own submissions | yes | yes | yes |
| matrix, CSV export, students list, any student page | no | yes | yes |
| any submission, its code, its full check log (build phase included) | no | yes | yes |
| queue: cancel, recheck | no | yes | yes |
| real logins on an anonymized leaderboard | no | yes | yes |
| score overrides | no | no | yes |
| token reset, SSH key deletion, deactivation, invites | no | no | yes |
| audit log | no | no | yes |
| git push to another account's repo or to the course repo | no | no | yes |

A TA gets the build-phase log because the person helping a student through a compile failure is the one who needs the compiler's words, and because a TA who may recheck already runs the hidden tests. Real logins on the leaderboard follow the same reasoning as for teachers: anonymization is there so students cannot rank each other. Anything a TA may not reach answers 404, not 403, exactly as it does for a student.

Every audited action records the role its actor held at the time, so the audit log tells a TA's recheck from a teacher's. Actions recorded before this existed show no role rather than a guessed one.

Rechecks: a commit message marker `[recheck <task-id>]` (works with an empty commit) or the recheck button on the task page. Student rechecks count against attempts and cooldown; teacher rechecks do not.

## Local self-check

`anygrade check` runs checks in the current working copy, open tests only, results to the terminal. It is the student self-check and course-authoring tool; it never fetches hidden tests.

```sh
anygrade check              # tasks changed against upstream/HEAD
anygrade check 01-intro     # named tasks
anygrade check --runner local -v
```

Exit codes: 0 all passed, 1 checks failed, 2 usage, 3 infrastructure (docker down etc.).

## Hidden tests

A task may pull extra tests from a private git repo or a local path at check time:

```yaml
hidden_tests:
  source: git             # git | local
  url: https://example.com/org/course-hidden.git
  ref: main               # branch or tag
  path: 01-intro/
```

The server caches hidden repos in the data dir and falls back to the last successful fetch when the remote is unreachable. Credentials come from the environment (`ANYGRADE_HIDDEN_GIT_TOKEN`, optional `ANYGRADE_HIDDEN_GIT_USER`) or the host's ssh agent, never from the course repo - `anygrade validate` rejects a `url` that embeds them. Hidden test contents and fetch errors never reach student-visible output.

With `source: local` the `path` is an absolute path on the grading server; `validate` warns when it is relative or missing locally, since a course repo is usually validated elsewhere. `ANYGRADE_HIDDEN_LOCAL_ROOTS` (colon-separated absolute roots) limits which directories such a path may reach; unset means unrestricted. Set it whenever the teachers who push `course.yaml` are not the administrators of the machine. `anygrade check` reads the working copy and is not subject to it.

### Keeping them off the disk while the solution runs

By default the hidden tests sit in the same workspace as the solution and run under the same uid, because the interpreter or compiler has to read them. A student who deliberately prints them reads them back out of their own check output.

For a **compiled** language you can close that. Give a check a `build:` phase:

```yaml
checks:
  - name: build            # the student's own code, so they see the compiler errors
    required: true
    weight: 0
    run: go build ./...
  - name: basic
    weight: 60
    build: go test -c -o $ANYGRADE_ARTIFACTS/basic.test ./...
    run: $ANYGRADE_ARTIFACTS/basic.test -test.run 'TestBasic'
  - name: advanced
    weight: 40
    build: go test -c -o $ANYGRADE_ARTIFACTS/advanced.test ./...
    run: $ANYGRADE_ARTIFACTS/advanced.test -test.run 'TestAdvanced'
```

Every `build:` runs first, then the hidden test files are removed from the workspace, then every `run:`. `go test -c` compiles the hidden tests together with the solution and **never executes** it, so by the time the test binary runs, the sources it was built from are gone. `$ANYGRADE_ARTIFACTS` is a directory in the workspace that survives the removal - it is where a build phase leaves what its run phase executes. Each phase gets the full `timeout`.

Read the three limits before you rely on this:

- **It is not secrecy against a determined student.** The test binary still carries test names, string literals and line numbers, and a process can read its own executable. You have turned `cat` into reverse engineering, which is worth doing, and not into a lock.
- **It buys nothing for an interpreted language.** Python, shell, Ruby: the test source has to be there when the student's code runs, so there is nothing to remove.
- **A `build:` that runs the solution defeats it.** `go test -run X ./...` executes student code with the hidden sources still on disk. Only compile in `build:`. `validate` cannot check this for you - the command is an arbitrary shell line - so it is on you.

The build phase's output is **staff-only**: it is the phase that compiles against the hidden tests, so a compiler error quoting a hidden source line would land in it. A student whose check fails there is told the build failed and nothing else, which is why the example above keeps a plain `go build ./...` gate: that one does not compile `_test.go` files, so its output is safe to show and it is where a student sees their own compile errors. Teachers and TAs get the build log next to the ordinary one on the submission page.

One check with a `build:` turns the boundary on for the whole task, so any check left run-only will not find the hidden tests any more. `anygrade validate` warns about each one; make sure it is what you meant.

## CLI

```
anygrade serve   [--repo DIR] [--data-dir DIR] [--http-addr :8080]
                 [--ssh-addr :2222] [--workers 4] [--base-url URL] [--local]
                 [--allow-local-runner] [--tls-cert FILE --tls-key FILE]
                 [--behind-proxy] [--retry-backoff 10s]
                 [--retry-backoff-cap 5m] [--max-retries 8]
anygrade check   [--runner local|docker] [--timeout D] [--keep] [-v] [TASK ...]
anygrade validate
anygrade user    add|list|remove|invite|reset-token|add-key ...
anygrade export  scores --format csv
                 submissions --task ID [--format dir|zip] [--out PATH]
                             [--all-attempts]
anygrade version
```

`serve --local` runs with a single implicit user and no auth for offline use; it refuses to bind to non-loopback addresses, so the listen addresses default to `127.0.0.1:8080` and `127.0.0.1:2222` in that mode.

A submission that fails on infrastructure - docker down, an image that will not pull, a hidden-tests remote that is unreachable with nothing cached - is not graded and not charged an attempt: it is retried with an exponential backoff, and becomes a terminal `infra_error` once the budget is spent. The schedule is `--retry-backoff` (first delay, doubling per retry), `--retry-backoff-cap` (upper bound on it) and `--max-retries` (how many retries before the row goes terminal); the defaults above are `10s`, `5m` and `8`, which is roughly twenty minutes of trying. Widen them for a course whose registry or hidden-tests remote is slow; a cap below the base, a non-positive delay or a zero budget is refused at startup. The schedule belongs to the deployment, so it is fixed for the life of the process - a teacher pushing `course.yaml` changes the course, never this.

Anything else should be served over TLS: either give `serve` a certificate (`--tls-cert` and `--tls-key`, both or neither), or put it behind a reverse proxy that terminates TLS and add `--behind-proxy`. Without one of the two the personal token - which is both the web login credential and the git password - crosses the network in the clear on every push and every login, and `serve` says so at startup. `--behind-proxy` is also what makes anygrade trust `X-Forwarded-Proto` and mark the session cookie `Secure`, and what makes the failed-login limiter read `X-Forwarded-For`. Set it whenever there really is a proxy: without it every request arrives from the proxy's address, so the whole course shares one budget and a few failed logins lock everyone out. Leave it off when there is not - both headers are forgeable by anyone who reaches the port.

## Plagiarism checks

anygrade does not compare solutions. It hands you the corpus and gets out of the way - similarity detection is a field with real tools in it, and a half-built one in here would be worse than none.

```
anygrade export submissions --task 01-intro --out /tmp/01-intro
```

writes one directory per student, holding only that task's `solution_files`, taken from the commit pinned at `refs/anygrade/submissions/<id>` - so a force push cannot rewrite what you are looking at:

```
/tmp/01-intro/
  _template/main.go     # the authoritative starter file, for the checker to subtract
  alice/main.go
  bob/main.go
  carol/main.go
```

The authoritative task files (open tests, `task.yaml`, build files) are identical in every submission by construction, so they are left out; they would be the strongest match in the run and would tell you nothing.

Each student contributes the submission the course scoring policy counts (`best` or `latest`) - the one their grade rests on. `--all-attempts` exports every recorded submission instead, as `alice@142` (the submission id, so a hit links straight back to `/submissions/142`). It catches the student who copied and then rewrote, and it multiplies the corpus by the number of pushes, which is why it is not the default.

`_template/` is the base code. Point the checker at it, or the shared skeleton dominates every pair:

```
# JPlag: --bc takes the name of a subdirectory of the root
java -jar jplag.jar -l go --bc=_template /tmp/01-intro

# MOSS: -b marks base files, one -b per file
moss -l cc -b /tmp/01-intro/_template/main.go /tmp/01-intro/*/main.go
```

A login can never start with `_`, so `_template` cannot be shadowed by a student directory. `--format zip --out corpus.zip` packs the identical tree into an archive (`--out -` streams it to stdout) for the upload forms that want one file.

The export reads the server's repos, so run it against the data dir (`--data-dir`, or from the course repo where `.anygrade/` lives). A student whose pinned commit has gone missing is named on stderr and the command exits non-zero; a solution file the student never committed is a warning, since grading used the template in its place and copying that into their tree would make every such student look identical.

## Security

- Student code is untrusted: the docker runner (one ephemeral container per submission) applies memory/cpu/pids limits, no network by default, read-only base image, a non-root user, a tmpfs workspace copied into the container instead of a host bind mount, and a hard wall-clock timeout. Serving on a non-loopback address with any task on the local runner refuses to start unless `--allow-local-runner` is passed explicitly.
- Tokens, invite links, session cookies and SSH key challenges are stored hashed; registering an SSH key takes a signed challenge, so nobody can claim a classmate's key; SSH is limited to git commands; failed logins are rate limited per client and login across git and the web.
- SSH has no guessable credential to rate limit - it authenticates a key fingerprint, and a client offers every key in its agent until one matches - so it is bounded on connection churn instead: how many connections may sit in the handshake at once, overall and per client address, how long each one has to get through it, and how long an established connection may sit idle. Nothing is tunable and nothing needs to be; the ceilings are far above a whole class pushing at a deadline, and a key that authenticates stops counting against them straight away.
- Rights checks on every route, and a refusal is a 404 rather than a 403 for every role alike: students can only read their own submissions, and a TA turned away from an account-management route learns no more about it than a student does. The check excerpt and the live stream are the student's; the full check log is a staff-only download, because their code runs beside the hidden tests. A check's `build:` phase is staff-only in full - no excerpt, no live stream - since that is the phase that compiles against the hidden tests.
- CSV export prefixes any cell starting with `=`, `+`, `-`, `@`, a tab or a carriage return with an apostrophe, so a login can never become a spreadsheet formula.

## Data directory

Everything lives in one directory, `./.anygrade` by default (`--data-dir` to override):

```
.anygrade/               # 0700, tightened at startup if the top level is wider
  anygrade.db            # SQLite, 0600 with its -wal/-shm siblings
  leaderboard.key        # 0600, the secret behind the leaderboard aliases
  repos/                 # bare course mirror + per-student repos
  hidden/                # hidden-tests cache
  logs/<submission-id>/  # raw check output
    build/               # build-phase output, staff-only
  workspaces/            # ephemeral check workspaces
```

Backup = copy the data dir, `leaderboard.key` included: it is what makes the anonymized leaderboard aliases stable. A missing key is regenerated and reshuffles every alias; a corrupt one stops the server with `not a hex-encoded secret; remove it to regenerate`. On restart, submissions that were running are re-queued and re-run from scratch.

Upgrading migrates the database in place, and one migration is not invisible: session rows cannot be converted to the hashed form they are now stored in, so they are dropped and everyone is signed out once. Accounts that carried more than one personal token keep the newest.

## Development

Requirements: Go 1.26+, the `git` binary, and optionally docker for the docker runner (colima works on macOS).

```sh
make check     # build + vet + gofmt + tests
make binary    # build ./anygrade
make e2e       # end-to-end regression suite (needs git, no docker)
```

## Releasing

A release is a pushed tag - everything else is automatic.

1. Dry-run locally - needs [goreleaser](https://goreleaser.com) (`brew install goreleaser`). `release-check` validates `.goreleaser.yaml`; `release-snapshot` builds every archive and `checksums.txt` into `dist/` without publishing anything. Unpack the archive for your platform and check that `anygrade version` reports the stamped metadata:

   ```sh
   make release-check
   make release-snapshot
   ls dist/
   ```

2. Tag `main` and push the tag:

   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

3. The `Release` workflow then runs the whole test suite, and only if it is green: builds linux/darwin × amd64/arm64, publishes the archives and `checksums.txt` with a changelog grouped by conventional commit type, and rebuilds the landing page so it advertises the new version.

   ```sh
   gh run watch                 # ci -> release -> pages
   gh release view v0.1.0
   ```

Tags are `vX.Y.Z`; archives drop the leading `v` (`anygrade_0.1.0_linux_amd64.tar.gz`). A pre-release tag (`v0.2.0-rc1`) is published as a pre-release: it does not become the latest release, so the landing page keeps pointing at the last stable one.

To take back a bad release, delete it and its tag, then fix and tag again:

```sh
gh release delete v0.1.0
git push --delete origin v0.1.0
```

The full specification is in [specs/SPEC.md](specs/SPEC.md).
