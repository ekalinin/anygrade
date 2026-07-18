# anygrade

A single Go binary that turns any git repository with course tasks into a grading system.

Point it at a course repo and it serves:

- a git interface for submitting solutions (SSH and smart HTTP),
- a web UI for students (results, live logs, scores) and teachers (matrix, overrides, CSV export, queue, audit), in English or Russian (`language:` in course.yaml, plus a per-user switcher),
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

Editing open tests, `task.yaml`, or build files in the student repo is useless: the authoritative versions are restored before checking, and such modifications are noted for the teacher. Pushes are never rejected for policy reasons - deadlines and attempt limits reject the submission, not the push.

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

registration:
  mode: invite            # invite | open
  # course_code: "go-2026"  # required when mode: open

scoring:
  policy: best            # best | latest - which submission counts per task

leaderboard:
  enabled: true
  anonymize: false        # true shows stable aliases instead of logins

defaults:                 # inherited by every task.yaml, overridable per task
  runner:
    type: docker          # docker | local
    image: golang:1.24
    timeout: 5m
    memory: 512m
    cpus: 1
    network: none
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

Raw score = `score × (passed weight / total weight)`. Late submissions between the soft and hard deadline get a percentage penalty per started interval; past the hard deadline they are recorded but not graded.

## Quick start

```sh
go build -o anygrade ./cmd/anygrade

# in the course repo: validate metadata and create the first teacher
./anygrade validate
./anygrade user add --login prof --role teacher

# serve (add .anygrade/ to the course repo's .gitignore)
./anygrade serve --http-addr :8080 --ssh-addr :2222
```

Students are registered by invite links (`anygrade user invite --login alice`, or `--csv roster.csv` for a whole group) or self-register with a course code when `registration.mode: open`. The activation page issues a personal token and prints the git setup. Two transports are available:

```sh
# over HTTP: username = login, password = the token
git clone http://host:8080/git/<login>/course.git
git remote add upstream http://host:8080/git/course.git

# or over SSH, once an SSH key is added (at activation or in settings)
git clone ssh://git@host:2222/<login>/course.git
git remote add upstream ssh://git@host:2222/course.git

# later: git pull upstream main
```

The token is the basic-auth password for git over HTTP and the login credential for the web UI. SSH auth is by key only; the token is not asked for. Teachers push course updates to `/git/course.git` - every push is validated and rejected with the error list if the metadata is broken.

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

The server caches hidden repos in the data dir and falls back to the last successful fetch when the remote is unreachable. Credentials come from the environment (`ANYGRADE_HIDDEN_GIT_TOKEN`, optional `ANYGRADE_HIDDEN_GIT_USER`) or the host's ssh agent, never from the course repo. Hidden test contents and fetch errors never reach student-visible output.

## CLI

```
anygrade serve   [--repo DIR] [--data-dir DIR] [--http-addr :8080]
                 [--ssh-addr :2222] [--workers 4] [--base-url URL] [--local]
                 [--allow-local-runner]
anygrade check   [--runner local|docker] [--timeout D] [--keep] [-v] [TASK ...]
anygrade validate
anygrade user    add|list|remove|invite|reset-token|add-key ...
anygrade export  scores --format csv
```

`serve --local` runs with a single implicit user and no auth for offline use; it refuses to bind to non-loopback addresses.

## Security

- Student code is untrusted: the docker runner (one ephemeral container per check) applies memory/cpu/pids limits, no network by default, read-only base image, and a hard wall-clock timeout. Serving on a non-loopback address with any task on the local runner refuses to start unless `--allow-local-runner` is passed explicitly.
- Tokens and invite links are stored hashed; SSH is limited to git commands; failed logins are rate limited per client and login across git and the web.
- Role checks on every route; students can only read their own submissions.

## Data directory

Everything lives in one directory, `./.anygrade` by default (`--data-dir` to override):

```
.anygrade/
  anygrade.db            # SQLite
  repos/                 # bare course mirror + per-student repos
  hidden/                # hidden-tests cache
  logs/<submission-id>/  # raw check output
  workspaces/            # ephemeral check workspaces
```

Backup = copy the data dir. On restart, submissions that were running are re-queued and re-run from scratch.

## Development

Requirements: Go 1.26+, the `git` binary, and optionally docker for the docker runner (colima works on macOS).

```sh
make check     # build + vet + gofmt + tests
make binary    # build ./anygrade
make e2e       # end-to-end regression suite (needs git, no docker)
```

The full specification is in [specs/SPEC.md](specs/SPEC.md).
