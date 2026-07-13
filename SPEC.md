# anygrade - specification

anygrade is a single Go binary that turns any git repository with course tasks into a grading system. Dropped into (or pointed at) a course repo, it reads metadata from the repo, and serves:

- a git interface for submitting solutions (SSH and smart HTTP),
- a web UI for students (results, logs, scores) and teachers (overview, export, adjustments),
- a local mode for offline self-checking and course authoring.

One running instance serves exactly one course.

## 1. Goals

- Work with any git repo of tasks: behavior is driven by metadata files (`course.yaml`, `task.yaml`), not by code changes in anygrade.
- Language-agnostic checking: a check is an arbitrary command; any language works if the environment (docker image or host) can run it.
- Familiar student flow: clone, edit, commit, push. Feedback starts right in the `git push` output.
- Single-binary deployment: SQLite storage, embedded web assets, only external dependencies are the `git` binary and (optionally) Docker.
- Hidden tests that students can never see, fetched from a private git repo or a local path at check time.

## 2. Non-goals (v1)

- Plagiarism detection. All solutions live in per-student git repos, so the data for later analysis is preserved. Future extension.
- Multi-course instances. Run one process per course.
- OAuth / external identity providers.
- Per-test-case result parsing (JUnit XML, `go test -json`). v1 scores by check groups via exit codes; parsers are a future extension.
- Horizontal scaling. One node, worker pool for parallel checks.
- LMS integrations (Moodle, Canvas). CSV export is the v1 integration point.

## 3. Concepts

| Term | Meaning |
|---|---|
| Course repo | The source git repo with tasks and metadata. The instance's single source of truth. |
| Student repo | A personal server-side clone of the course repo, created per student. Students clone and push only their own repo. |
| Upstream | The course repo exposed read-only to students, added as a second remote for pulling task updates. |
| Task | A directory in the course repo containing a `task.yaml`. |
| Submission | One graded unit: (student, task, commit) received via push or UI recheck. |
| Check | A named command from `task.yaml` with a weight; a submission runs the task's list of checks. |
| Solution files | Per-task allowlist of paths a student is supposed to change. Everything else is restored from the course repo before checking. |

## 4. Repository layout and metadata

### 4.1 Course repo layout

```
course-repo/
  course.yaml          # course-level config and defaults
  tasks/
    01-intro/
      task.yaml        # task config: checks, score, deadline, limits
      README.md        # task statement (any language, teacher's choice)
      main.go          # solution template
      main_test.go     # open tests, visible to students
    02-structs/
      task.yaml
      ...
```

The tasks root directory is configurable; a task is any directory under it containing `task.yaml`.

### 4.2 course.yaml

```yaml
name: "Go course 2026"
tasks_dir: tasks

registration:
  mode: invite            # invite | open
  course_code: "go-2026"  # required when mode: open

leaderboard:
  enabled: true
  anonymize: false        # true shows aliases instead of logins

scoring:
  policy: best            # best | latest - which submission counts per task

defaults:                 # inherited by every task.yaml, overridable per task
  runner:
    type: docker          # docker | local
    image: golang:1.24
    timeout: 5m
    memory: 512m
    cpus: 1
    network: none
  limits:
    max_attempts: 0       # 0 = unlimited
    cooldown: 0s
  deadline:
    penalty:
      percent: 10
      per: 24h
      max_percent: 50
  workspace:
    include:              # extra repo-relative paths exported into every check
      - go.mod            # workspace (unioned with per-task workspace.include)
```

### 4.3 task.yaml

```yaml
id: 01-intro              # optional, defaults to directory name
name: "Intro task"
score: 100

solution_files:           # allowlist of student-editable paths (relative to task dir)
  - main.go

deadline:                 # all fields optional; absent = no deadline
  soft: 2026-09-24T23:59:59+03:00
  hard: 2026-10-01T23:59:59+03:00
  penalty:                # applied between soft and hard
    percent: 10
    per: 24h
    max_percent: 50

limits:
  max_attempts: 20
  cooldown: 5m

runner:                   # overrides course defaults
  type: docker
  image: golang:1.24
  timeout: 5m

hidden_tests:             # optional
  source: git             # git | local
  url: https://example.com/org/course-hidden.git
  ref: main
  path: 01-intro/         # subdirectory inside the hidden repo
  # source: local
  # path: /srv/hidden/01-intro

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

Semantics:

- Timestamps are RFC 3339 with explicit offsets.
- Checks run in order. A check passes iff its command exits 0.
- `required: true` checks are gates: on failure the remaining checks are skipped and the raw score is 0.
- Weights are normalized over the non-gate checks: raw score = score × (sum of passed weights / sum of all weights). A single check with any weight therefore behaves as all-or-nothing.
- `workspace.include` (course defaults and per-task `workspace:` block, unioned) lists extra repo-relative paths - files or directories - exported into the check workspace alongside the task directory. Needed when tasks share build files, e.g. a course-root `go.mod`. Paths must exist and must not escape the repo.
- `anygrade validate` verifies all metadata (unknown fields, missing files in `solution_files`, deadline ordering, duplicate task ids) and is also run at server startup; startup fails on invalid metadata.

## 5. Architecture

```
                 ┌────────────────────────────────────────────┐
                 │                 anygrade                    │
 git ssh ───────▶│ ssh server ─┐                               │
 git http ──────▶│ http git    ├─▶ receive hook ─▶ submission  │
 browser ───────▶│ web UI (SSR + htmx + SSE)        queue      │
                 │                                  (SQLite)   │
                 │                          worker pool        │
                 │                       ┌──────┴──────┐       │
                 │                    local runner  docker     │
                 │                                  runner     │
                 └────────────────────────────────────────────┘
   data dir: SQLite DB, bare student repos, hidden-tests cache, logs
```

Components:

- git server: system `git` binary does the protocol work (`upload-pack` / `receive-pack`), anygrade wraps it for auth, routing, and the post-receive submission hook. Same approach as Gitea; maximum client compatibility. `go-git` is not used.
- submission queue: a table in SQLite; a configurable worker pool claims queued submissions. Nothing is lost on restart: submissions in `running` state are returned to `queued` at startup.
- runners: `local` executes check commands as host processes (trusted/local use only), `docker` runs each submission's checks in a container with cpu/memory/timeout limits and no network by default.
- storage: SQLite (WAL mode) in the data dir. Check logs are files on disk; the DB stores paths and excerpts.
- web UI: Go `html/template` + htmx, SSE for live status and log streaming, all assets via `go:embed`.

### 5.1 Data dir

Default `./.anygrade` next to the course repo (must be gitignored), overridable with `--data-dir`.

```
.anygrade/
  anygrade.db            # SQLite
  repos/
    course.git           # bare mirror of the course repo (upstream)
    students/<login>.git # bare per-student repos
  hidden/<hash>/         # cached clones of hidden-test repos
  logs/<submission-id>/  # raw check output
  workspaces/            # ephemeral check workspaces
```

## 6. Submission flow

1. Student pushes to their personal repo (SSH or HTTP).
2. Pre-receive: reject only protocol-level problems (push exceeding `max_push_size`, pushes to reserved refs). Deadlines and limits never reject a push - the repo belongs to the student.
3. Post-receive: the server diffs the new branch head against the student's last processed commit (baseline). Changed paths are mapped to task directories.
4. For each changed task, a submission is created, unless skipped by policy:
   - hard deadline passed → submission recorded with status `rejected_deadline`, not queued;
   - `max_attempts` exhausted or `cooldown` active → recorded as `rejected_limit`, not queued.
5. Push output (sideband, `remote:` lines) reports what happened:

   ```
   remote: anygrade: 2 task(s) detected
   remote:   01-intro   submission #142 queued   http://host/submissions/142
   remote:   02-structs rejected: hard deadline passed (2026-10-01 23:59 +03)
   ```

   Pushes that change no task directories get `remote: anygrade: no tasks changed`.
6. A worker picks the submission up, assembles a workspace, runs checks, stores results. The student watches progress live in the UI.
7. A hidden ref `refs/anygrade/submissions/<id>` is created at the submitted commit so graded commits survive force pushes and stay auditable.

Explicit recheck (in addition to diff detection):

- commit message marker `[recheck <task-id>]` (works with an empty commit),
- a recheck button in the UI on the task page.

Student-initiated rechecks count against `max_attempts` and `cooldown`; teacher-initiated rechecks do not.

Submission time is the moment the server receives the push (server clock). Commit timestamps are never trusted.

### 6.1 Workspace assembly (anti-cheat)

For each submission the worker builds a clean workspace:

1. Export the task directory from the course repo at its current head (the authoritative version - templates, open tests, build files, `task.yaml`), plus any `workspace.include` paths (shared build files such as a course-root `go.mod`), mirroring the repo-relative layout.
2. Copy only `solution_files` from the student's submitted commit on top.
3. If hidden tests are configured, sync the source (git: fetch into the cache, use last successful cache if the remote is unreachable; local: read the path) and copy them on top.
4. Run checks inside this workspace.

Consequences:

- Editing open tests, `task.yaml`, or build files in the student repo is useless: they are silently replaced by the authoritative versions. Such modifications are noted in the submission log for the teacher.
- Checks always run against the current course-repo version of the task, so students do not need to pull upstream for their existing solutions to be graded correctly (they pull to get new tasks and updated templates).

## 7. Git server

- Per-student bare repos are created lazily at account activation as clones of the course repo.
- Students have read/write access to their own repo only, and read-only access to the upstream course repo.
- Suggested student setup (printed on the invite/activation page):

  ```
  git clone http://host/git/<login>/course.git
  git remote add upstream http://host/git/course.git
  # later: git pull upstream main
  ```

- Task updates are the student's responsibility: `git pull upstream main`, resolving conflicts locally. The server never merges into student repos.
- Force pushes to student repos are allowed; grading history is preserved via hidden submission refs.
- Teachers update the course repo by pushing to the upstream repo (teachers get write access to it); on each update the server re-validates metadata and refreshes the bare mirror. If validation fails the push is rejected with the error list.

Endpoints:

- smart HTTP: `http(s)://host/git/<login>/course.git` and `/git/course.git`, basic auth with login + personal token; served on the same port as the web UI.
- SSH: `ssh://git@host:2222/<login>/course.git`, auth by registered public keys; single system user, key lookup by fingerprint.

## 8. Authentication and registration

Three modes, all supported:

- HTTP + personal access token: generated at activation, shown once, stored hashed. Used as basic-auth password for git HTTP and to log into the UI (session cookie after login). Tokens can be regenerated by the student (self) or reset by the teacher.
- SSH keys: uploaded during activation or later in the UI; multiple keys per student.
- No auth (local mode only): `serve --local` runs with a single implicit user and no login; git endpoints are open. Refuses to bind to non-loopback addresses.

Registration (configured in `course.yaml`):

- `invite`: the teacher loads a roster (CLI `anygrade user add` or a CSV import); the system generates one-time invite links. A student opens the link, sets up a token and/or SSH key, and gets their repo URL.
- `open`: students self-register with the course code; the teacher can deactivate accounts.

Roles: `student` and `teacher`. Teachers see everything, adjust scores, manage users, trigger rechecks, export CSV. The first teacher account is created via CLI.

## 9. Scoring and deadlines

- Raw score: `score × (passed weight / total weight)`, with gates as described in 4.3.
- Deadline policy per task: none, hard only, soft+hard, or soft only (soft only means penalties accrue up to `max_percent` and submissions stay accepted).
  - on-time (≤ soft, or no soft): no penalty;
  - late (soft < t ≤ hard): `penalty.percent` per each started `penalty.per` interval after soft, capped at `max_percent`;
  - past hard: submission not graded (`rejected_deadline`).
- Final task score: `best` or `latest` submission per `scoring.policy` (course-wide, default `best`). Penalty is computed per submission at its submission time.
- Teachers can set a manual score override per (student, task) with a comment; overrides win over computed scores and are visible in the audit log.

## 10. Web UI

Server-rendered pages, htmx for interactivity, SSE for live updates.

Student pages:

- dashboard: task list with status (not started / queued / running / passed / partial / failed / rejected), scores, deadlines with countdowns;
- task page: statement (rendered task README), submission history with attempts left/cooldown state, recheck button;
- submission page: per-check results, penalty breakdown, live log stream while running, full logs after.

Teacher pages:

- matrix: students × tasks with scores and statuses, filters, click-through to any submission and its code (view of the submitted commit);
- student page: all submissions, token/key management, deactivate;
- score adjustment with comment;
- CSV export of the score matrix;
- queue view: pending/running checks, cancel, recheck.

Leaderboard (if enabled): total scores ranked; `anonymize` replaces logins with stable aliases. Visible to all authenticated users.

## 11. CLI

```
anygrade serve   [--repo DIR] [--data-dir DIR] [--http-addr :8080]
                 [--ssh-addr :2222] [--workers 4] [--base-url URL] [--local]
                 [--allow-local-runner]
anygrade check   [--runner local|docker] [--timeout D] [--keep] [-v] [TASK ...]
                              # run checks locally in the current working copy,
                              # open tests only, results to the terminal; exit
                              # codes: 0 all passed, 1 checks failed, 2 usage,
                              # 3 infrastructure (docker down etc.)
anygrade validate             # validate course.yaml and all task.yaml files
anygrade user    add|list|remove|invite|reset-token ...
anygrade export  scores --format csv
```

- `check` with no arguments detects tasks changed against upstream/HEAD; with arguments checks the named tasks. It uses the same runner code path as the server (docker or local per metadata) but never fetches hidden tests unless they are locally available - it is the student self-check and course-authoring tool.
- `check --runner local` overrides the per-task runner for students without docker; it runs task code unsandboxed on the host, which is acceptable at the self-check trust level (own machine, own code). No `--allow-local-runner` gate applies here - that gate is only for a non-loopback server.
- Secrets (hidden-tests repo credentials) come from the environment (`ANYGRADE_HIDDEN_GIT_TOKEN`) or standard git credential helpers, never from the course repo.

## 12. Storage model (sketch)

SQLite tables:

- `users` (id, login, display_name, role, state, created_at)
- `tokens` (user_id, hash, created_at, last_used_at)
- `ssh_keys` (user_id, fingerprint, public_key, created_at)
- `invites` (token_hash, user_id, expires_at, used_at)
- `submissions` (id, user_id, task_id, commit_sha, received_at, attempt_no, status: queued|running|done|infra_error|rejected_deadline|rejected_limit, raw_score, penalty_percent, final_score, log_dir, worker_note)
- `check_results` (submission_id, name, passed, exit_code, duration_ms, weight, log_excerpt)
- `score_overrides` (user_id, task_id, score, comment, teacher_id, created_at)
- `events` (audit log: user/teacher actions)

Task definitions are not mirrored into the DB; metadata is always read from the course repo (current head), so the repo stays the single source of truth. Submissions reference tasks by id; results for deleted tasks are kept but hidden from scoring.

## 13. Edge cases

- Multiple tasks in one push: one submission per changed task, queued independently.
- Rapid successive pushes to the same task: each becomes a submission (subject to cooldown); they run in order; `scoring.policy` decides what counts.
- Server restart: `running` submissions are requeued and rerun from scratch; check runs must therefore be idempotent (fresh workspace each time).
- Docker daemon down / image pull failure / hidden repo unreachable with no cache: submission gets `infra_error`, does not consume an attempt, is automatically retried with backoff, and is surfaced in the teacher queue view.
- Check timeout: the container/process is killed, the check fails with a `timed out after 5m` note; remaining checks still run unless the timed-out check was a gate.
- Push touching only non-task files: accepted, no submissions, informational push message.
- Task deleted or renamed in the course repo: pending submissions for unknown task ids fail with a clear error; historical results remain visible.
- Student pushes a branch other than the default: accepted and stored, but only default-branch pushes create submissions (stated in the push output).
- Clock and timezones: all comparisons in UTC on the server clock; deadlines carry explicit offsets; UI renders in the course timezone.
- `max_push_size` (default 50 MB) guards against giant blobs; oversized pushes are rejected pre-receive with an explanatory message.
- Very long logs: stored complete on disk, truncated to a configurable excerpt (default 64 KB per check) in the DB/UI with a full-log download link.

## 14. Security considerations

- Student code is untrusted. The docker runner is the only mode suitable for a public service: no network (unless the task opts in), cpu/memory/pids limits, read-only base image, non-root user, tmpfs workspace, hard wall-clock timeout. `serve` on a non-loopback address with any task resolved to the local runner refuses to start unless `--allow-local-runner` is passed explicitly; on loopback (including `serve --local`) no flag is needed. The local runner enforces only the wall-clock timeout (process-group kill); memory/cpu limits are docker-only, and `validate` warns when a local-runner task sets them.
- Hidden tests never enter student-visible repos, push outputs, or student-visible logs. Check logs are shown to students as produced by their tests; teachers see full logs. Course authors are advised to keep hidden-test source files out of error output.
- Tokens and invite links are stored hashed; SSH is limited to git commands (no shell).
- The web UI enforces role checks on every route; students can only read their own submissions.

## 15. Key decisions and tradeoffs

| Decision | Chosen | Rationale / tradeoff |
|---|---|---|
| Submission model | Per-student server-side repo | Full isolation and per-student history; costs disk space vs shared-repo branches |
| Git protocol impl | System `git` binary | Battle-tested protocol handling (Gitea approach); adds a host dependency that is almost always present |
| Task detection | Diff against last processed commit + explicit `[recheck]` | Zero extra student actions; explicit marker covers re-runs and edge cases |
| Anti-cheat | Restore authoritative files, allowlist solution files | Pushes are never rejected; tampering is simply ineffective and logged |
| Course updates | Student pulls from upstream | Standard git flow; conflicts resolved by the code owner, server never merges |
| Feedback | Async: push output + UI with SSE | Immediate acknowledgement without hanging pushes on long test runs |
| Scoring | Weighted check groups | Partial credit without per-test output parsing; one group degrades to all-or-nothing |
| Storage | SQLite + files for logs | Single-binary ops, transactions, easy backup; enough for course-sized load |
| UI | SSR + htmx + SSE, embedded | No frontend toolchain; SPA/JSON API deferred |
| Scope | 1 instance = 1 course, student+teacher roles | Simplest possible model; multi-course via multiple instances |

## 16. Future work

- Plagiarism detection (or an export hook for MOSS/JPlag).
- Per-test-case parsers (`go test -json`, JUnit XML, TAP) for finer UI detail and proportional scoring.
- TA role with limited teacher rights.
- JSON API as a stable contract for scripts and bots.
- OAuth login.
- Webhooks/notifications (Telegram, email) on check completion.

## 17. Implementation milestones

1. Metadata: schemas, loader, `anygrade validate`.
2. Runner core: workspace assembly, local + docker runners, `anygrade check` (local CLI value works before any server exists).
3. Storage and queue: SQLite schema, worker pool, restart recovery.
4. Git server: smart HTTP + SSH, per-student repos, receive hook, push feedback.
5. Web UI: auth, student pages, SSE live updates.
6. Teacher features: matrix, overrides, CSV export, queue view, invites/registration.
7. Hardening: limits, deadlines/penalties, hidden-tests cache, edge cases from section 13.
