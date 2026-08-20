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
language: en              # web UI language: en | ru (default en)
timezone: Europe/Berlin   # IANA name the UI renders times in (default UTC)

registration:
  mode: invite            # invite | open
  course_code: "go-2026"  # required when mode: open

leaderboard:
  enabled: true
  anonymize: false        # true shows aliases instead of logins

scoring:
  policy: best            # best | latest - which submission counts per task

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
    max_file_size: 10m    # per solution file copied out of the student commit
    max_total_size: 64m   # all solution files of one submission together (64m)
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
  url: https://example.com/org/course-hidden.git   # never with credentials in it
  ref: main
  path: 01-intro/         # subdirectory inside the hidden repo
  # source: local
  # path: /srv/hidden/01-intro   # absolute path on the grading server

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
- Weights must be `>= 0`. Because they are normalized, a negative one pushes the raw score outside `0..score` - weights 60 and -40 score 300 out of 100 - so `validate` rejects a negative weight on a non-gate check. Weight 0 stays legal and is what gates carry.
- `workspace.include` (course defaults and per-task `workspace:` block, unioned) lists extra repo-relative paths - files or directories - exported into the check workspace alongside the task directory. Needed when tasks share build files, e.g. a course-root `go.mod`. Paths must exist and must not escape the repo.
- `hidden_tests.url` must not embed credentials: they come from the environment (§11). With `source: local` the `path` is an absolute path on the machine that runs the checks, which is why `validate` only warns - never errors - when it is relative or absent locally: a course repo is usually validated somewhere other than the grading server.
- `anygrade validate` verifies all metadata (unknown fields, missing files in `solution_files`, symlinked entries in `solution_files`/`workspace.include`, deadline ordering, duplicate task ids, negative check weights, non-positive size limits, credentials embedded in a hidden-tests url) and is also run at server startup; startup fails on invalid metadata. Warnings are reported by `validate` alone; they never fail a startup or a teacher push.

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
.anygrade/               # created 0700; a wider top-level mode is tightened at startup
  anygrade.db            # SQLite, 0600 with its -wal/-shm siblings
  anygrade.sock          # intake socket the receive hooks talk to, owner-only
  leaderboard.key        # 0600, per-instance secret behind the leaderboard aliases
  repos/                 # 0700
    course.git           # bare mirror of the course repo (upstream)
    students/<login>.git # bare per-student repos
  hidden/<hash>/         # cached clones of hidden-test repos, 0700
  logs/<submission-id>/  # raw check output, 0700
  workspaces/            # ephemeral check workspaces, 0700
```

A submission's log dir holds one file per check, named after the check: a name that is already file-name safe keeps its spelling (`build.log`), anything else - uppercase included, since macOS is case-insensitive by default - is sanitized and tagged with a hash of the original (`Build~<hash>.log`), so two checks of one task can never write to the same file.

## 6. Submission flow

1. Student pushes to their personal repo (SSH or HTTP).
2. Only protocol-level problems reject a push: a pack exceeding `max_push_size` (the transport stops reading it) or a push to a reserved ref (pre-receive). Deadlines and limits never reject a push - the repo belongs to the student.
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

A marker only applies to the push that carried it. If the server fails to record a push at all, the push output says so, and the next push re-detects the changed content against the baseline - but the `[recheck <task-id>]` marker is lost and has to be repeated.

Submission time is the moment the server receives the push (server clock). Commit timestamps are never trusted.

### 6.1 Workspace assembly (anti-cheat)

For each submission the worker builds a clean workspace:

1. Export the task directory from the course repo at its current head (the authoritative version - templates, open tests, build files, `task.yaml`), plus any `workspace.include` paths (shared build files such as a course-root `go.mod`), mirroring the repo-relative layout.
2. Copy only `solution_files` from the student's submitted commit on top, without following symlinks and within `workspace.max_file_size` / `max_total_size`.
3. If hidden tests are configured, sync the source (git: fetch into the cache, use last successful cache if the remote is unreachable; local: read the path) and copy them on top, read-only.
4. Run checks inside this workspace.

Consequences:

- Editing open tests, `task.yaml`, or build files in the student repo is useless: they are silently replaced by the authoritative versions. Such modifications are noted in the submission log for the teacher.
- Checks always run against the current course-repo version of the task, so students do not need to pull upstream for their existing solutions to be graded correctly (they pull to get new tasks and updated templates).
- Symlinks are never materialized. Both the course-repo export and the copy from a working copy skip them with a log line, so a workspace is always a plain tree.
- The size limits bound the decompressed overlay, which `max_push_size` - a limit on the compressed pack - cannot. A submission whose solution file is a symlink, escapes the workspace, or exceeds either limit fails terminally, with the reason in the worker note; it is not retried.
- Hidden tests are copied in with their write bits stripped, so one check cannot rewrite the tests the next check runs against. This is not an isolation boundary: the checks run as the owner of those files (§14).

## 7. Git server

- Per-student bare repos are clones of the course repo, created at account activation. The first git access creates one too, as a fallback: accounts made with `anygrade user add` never go through an activation page, and the activation itself must not fail on a slow clone once the invite is already spent.
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

- HTTP + personal access token: generated at activation, shown once, stored hashed. Used as basic-auth password for git HTTP and to log into the UI (session cookie after login). Tokens can be regenerated by the student (self) or reset by the teacher. The session cookie is marked `Secure` when the connection is actually TLS, or when `--behind-proxy` is set and the proxy sends `X-Forwarded-Proto: https`.
- SSH keys: uploaded during activation or later in the UI; multiple keys per student. Registration is first-come-first-served and proves nothing about possession: a fingerprint already held by another account is refused, audited as `key.duplicate` naming both accounts, and can be removed by a teacher from the holder's student page. Proof of possession is future work (§16).
- No auth (local mode only): `serve --local` runs with a single implicit user and no login; git endpoints are open. Refuses to bind to non-loopback addresses, and therefore defaults `--http-addr`/`--ssh-addr` to the loopback interface when they are not given.

Registration (configured in `course.yaml`):

- `invite`: the teacher creates the accounts and the students activate them. Two CLI commands cover the two ways an account can start (§11):
  - `anygrade user invite` creates the account and prints a one-time link, for one login or for a whole roster via `--csv`. The student opens the link, sets up a token and/or SSH key, and gets their repo URL. This is the normal path. The link is consumed before the account is activated, so a failed activation never leaves a reusable link behind - the teacher issues a new one;
  - `anygrade user add` creates the account and issues its personal token right away, printed once. There is no link and no activation page - the teacher hands the token over. This is what the first teacher account needs, since nobody exists yet to invite it, and what scripted setups use when no browser is involved.
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

Leaderboard (if enabled): total scores ranked, visible to all authenticated users. `anonymize` replaces logins with stable aliases **for students**; teachers keep seeing logins. An alias is a keyed hash of the login over a per-instance secret kept in the data dir (`leaderboard.key`): stable for as long as that file lives, and not reproducible outside the server. A missing file is regenerated, which reshuffles every alias; a file that is not a hex-encoded secret aborts startup with an instruction to remove it and regenerate, rather than silently reshuffling the board. Anonymization exists so students cannot read each other's standings off the board, and §8 gives teachers full visibility anyway - hiding the names from them would only send them to the matrix for the same information, one click away.

### 10.1 Localization

The web UI ships English and Russian message catalogs (`internal/i18n/locales/*.yaml`; English is the source of truth, other locales fall back to it key by key). Adding a locale is a new `<code>.yaml` with the full key set - no code change.

The active locale is chosen per request: a valid `ag_lang` cookie wins, then the course default (`language:` in course.yaml, `en` if unset). A language switcher in the top bar sets the cookie and works on the anonymous pages (login, register, invite) too, so it needs no account or DB column.

Scope is the web UI only: pages, flash/error messages, and status badge labels. Git push output, CLI output, and server logs stay English. Strings written into the database at submission time - worker tamper notes, deadline/attempt reject reasons, the scrubbed hidden-tests message - are stored in English and rendered as-is regardless of locale.

## 11. CLI

```
anygrade serve   [--repo DIR] [--data-dir DIR] [--http-addr :8080]
                 [--ssh-addr :2222] [--workers 4] [--base-url URL] [--local]
                 [--allow-local-runner] [--tls-cert FILE --tls-key FILE]
                 [--behind-proxy]
anygrade check   [--runner local|docker] [--timeout D] [--keep] [-v] [TASK ...]
                              # run checks locally in the current working copy,
                              # open tests only, results to the terminal; exit
                              # codes: 0 all passed, 1 checks failed, 2 usage,
                              # 3 infrastructure (docker down etc.)
anygrade validate             # validate course.yaml and all task.yaml files
anygrade user    add|list|remove|reset-token|add-key|invite ...
                              # add: create an account and issue its token now,
                              #      shown once (first teacher, scripted setup)
                              # invite: create an account and print a one-time
                              #      activation link; --csv for a whole roster
anygrade export  scores --format csv
```

- `check` with no arguments detects tasks changed against upstream/HEAD; with arguments checks the named tasks. It uses the same runner code path as the server (docker or local per metadata) but never fetches hidden tests unless they are locally available - it is the student self-check and course-authoring tool.
- `check --runner local` overrides the per-task runner for students without docker; it runs task code unsandboxed on the host, which is acceptable at the self-check trust level (own machine, own code). No `--allow-local-runner` gate applies here - that gate is only for a non-loopback server.
- `--tls-cert` and `--tls-key` are required together and make the web/git HTTP listener serve HTTPS. `--behind-proxy` trusts `X-Forwarded-Proto` from a proxy that terminates TLS instead, and is also what makes the failure limiter read `X-Forwarded-For`: without it every request behind a proxy shares one client address, and a few failed logins would exhaust the per-IP budget for the whole course. Both headers are forgeable by anyone who reaches the port, which is why neither is read without the flag. Without one of the two the token travels in the clear (§14).
- Secrets (hidden-tests repo credentials) come from the environment (`ANYGRADE_HIDDEN_GIT_TOKEN`) or standard git credential helpers, never from the course repo; `validate` enforces that rule by rejecting a `hidden_tests.url` with credentials embedded in it.
- `ANYGRADE_HIDDEN_LOCAL_ROOTS` is a colon-separated list of absolute roots (a relative entry is ignored and reported, so it can only narrow the list) that `hidden_tests: source: local` may read from; unset means unrestricted. Recommended whenever the teachers who push `course.yaml` are not the administrators of the machine, since a local hidden-tests path otherwise reaches any directory the server can read. `anygrade check` reads the working copy and is not subject to it.
- `export scores --format csv`, and the same export in the teacher UI, prefix a cell that starts with `=`, `+`, `-`, `@`, a tab or a carriage return with an apostrophe, so a login or task id cannot become a formula in a spreadsheet.

## 12. Storage model (sketch)

SQLite tables:

- `users` (id, login, display_name, role, state, created_at)
- `tokens` (user_id, hash, created_at, last_used_at) - one active token per account: `user_id` is unique and a rotation is an upsert, so two racing rotations cannot leave two valid tokens behind. An account that already carried duplicates keeps its newest token
- `ssh_keys` (user_id, fingerprint, public_key, created_at)
- `invites` (token_hash, user_id, expires_at, used_at)
- `sessions` (id_hash, user_id, token_hash, created_at, expires_at) - web sessions; the cookie value is stored hashed, like tokens and invites, so the table is not a set of usable cookies. A hash cannot be derived from the values already stored, so the migration that introduced it empties the table: an upgrade signs everybody out once
- `pushes` (id, user_id, ref, old_sha, new_sha, received_at, processed_at) - the intake log: every accepted push to a graded branch, recorded on arrival and graded afterwards, so a push is an event with its own boundaries and arrival time rather than a ref position
- `submissions` (id, user_id, task_id, commit_sha, received_at, attempt_no, counts, status: queued|running|done|infra_error|rejected_deadline|rejected_limit, raw_score, penalty_percent, final_score, log_dir, worker_note, retries, retry_at, started_at, canceled_at)
- `check_results` (submission_id, name, passed, exit_code, duration_ms, weight, skipped, timed_out, log_excerpt)
- `score_overrides` (user_id, task_id, score, comment, teacher_id, created_at)
- `events` (audit log: user/teacher actions)

There is no `canceled` status. A cancel writes `infra_error` with `retry_at` cleared, `counts` set to 0, `canceled_at` stamped and the worker note `canceled by teacher` - which is exactly what keeps the retry loop from re-arming it and the attempt from being consumed (§13).

Task definitions are not mirrored into the DB; metadata is always read from the course repo (current head), so the repo stays the single source of truth. Submissions reference tasks by id; results for deleted tasks are kept but hidden from scoring.

## 13. Edge cases

- Multiple tasks in one push: one submission per changed task, queued independently.
- Rapid successive pushes to the same task: each becomes a submission (subject to cooldown); they run in order; `scoring.policy` decides what counts. The order holds across retries too - a submission waiting on an `infra_error` backoff holds back the later submissions of the same (student, task) pair, and nothing else: other students and other tasks keep running.
- Server restart: `running` submissions are requeued and rerun from scratch; check runs must therefore be idempotent (fresh workspace each time).
- Docker daemon down / image pull failure / hidden repo unreachable with no cache: submission gets `infra_error`, does not consume an attempt, is automatically retried with backoff, and is surfaced in the teacher queue view.
- Teacher cancel is final: it stops the run whichever moment it arrives in, and a retry already in flight never re-arms the canceled submission.
- Check timeout: the container/process is killed, the check fails with a `timed out after 5m` note; remaining checks still run unless the timed-out check was a gate.
- Push touching only non-task files: accepted, no submissions, informational push message.
- Task deleted or renamed in the course repo: pending submissions for unknown task ids fail with a clear error; historical results remain visible.
- Student pushes a branch other than the default: accepted and stored, but only default-branch pushes create submissions (stated in the push output).
- Clock and timezones: all comparisons in UTC on the server clock; deadlines carry explicit offsets; UI renders in the course timezone.
- `max_push_size` (course-wide, default 50 MB) guards against giant blobs. The server stops reading the pack itself, on top of git's own `receive.maxInputSize`, and the rejection is anygrade's own message: it names the limit and says how to recover (drop the large files from the commit and push again). A teacher pushing a new value gets it applied without a restart.
- Very long logs: the on-disk log is capped at `runner.log_max` (default 10 MB per check) and ends with an explicit truncation marker; the excerpt in the DB/UI (default 64 KB per check) carries the same marker. A log the server could not write does not fail the check - the excerpt says the full log is missing. The full log is a teacher-only download (§14).

## 14. Security considerations

Student code is untrusted:

- The docker runner is the only mode suitable for a public service: no network (unless the task opts in), cpu/memory/pids limits, read-only base image, non-root user, tmpfs workspace, hard wall-clock timeout. Student containers never run as root: when anygrade itself runs as root, the container falls back to a fixed unprivileged uid/gid and the workspace is chowned to it.
- `serve` on a non-loopback address with any task resolved to the local runner refuses to start unless `--allow-local-runner` is passed explicitly; on loopback (including `serve --local`) no flag is needed. The same gate is re-applied to every course snapshot, so a teacher push that would move a task onto the local runner on a public bind is rejected and the previously validated snapshot stays active.
- The local runner enforces only the wall-clock timeout (process-group kill); memory/cpu limits are docker-only, and `validate` warns when a local-runner task sets them. It has no privilege drop either: checks run as whatever user anygrade runs as.
- A submission cannot cost the server unbounded disk or memory: the student overlay is bounded by `workspace.max_file_size` / `max_total_size` and each check log by `runner.log_max` (§6.1, §13). `max_push_size` bounds the compressed pack only, so every path that reads a blob afterwards carries its own limit - including the teacher code view, which refuses a file too large to hold in memory rather than reading it.

Hidden tests are confidential against the network and the UI, not against the code they test:

- They never enter student-visible repos, push output, or the student-visible parts of the UI, and every hidden-tests failure a student can see is scrubbed to a fixed message.
- Check logs are shown to students as produced by their tests - the stored excerpt and the live stream - while the raw full-log download is teacher-only, because student code runs beside the hidden tests.
- Within a single check run the sandbox is the boundary, not the filesystem. Hidden tests are placed read-only, but they sit in the same workspace as the solution and run under the same uid, because the compiler or interpreter has to read them; a student who deliberately dumps them into their own output reads them back through the excerpt and the live stream. Course authors are advised to keep hidden-test sources out of error output. A course whose hidden tests must stay secret against a determined student needs a per-check execution boundary that anygrade does not implement today (§16).

Credentials and transport:

- Tokens, invite links and session cookies are stored hashed; SSH is limited to git commands (no shell).
- Without TLS the personal access token crosses the network in the clear on every push and every login, since the same token is the git basic-auth password and the web credential. `serve` warns at startup on a non-loopback plaintext bind; run it with `--tls-cert`/`--tls-key`, or behind a proxy that terminates TLS with `--behind-proxy`.
- Failed credential checks are budgeted per (client IP, login) and per client IP, shared between the web login form and git HTTP basic auth; open registration's course code draws from the same per-IP budget.
- The SSH transport is budgeted on churn, not on credentials, and deliberately shares nothing with the failure limiter above. There is no guessable credential to budget: SSH authenticates a public-key fingerprint, and an honest client offers every key in its agent until one matches, so counting the misses would throttle students without slowing an attacker down (the offers are already capped at six per connection by the SSH library, one indexed lookup each). What is bounded is what a peer can hold before it has a name: at most 512 connections may sit in the SSH handshake at once, at most 64 of them from one client address, and each has 2 minutes to get through it - `sshd`'s own `LoginGraceTime` default, which leaves room for the client's key-passphrase prompt. Those two together cap the server's whole unauthenticated footprint, because a registered key hands its slot back before the transfer starts; a lab section pushing from one NAT address at a deadline occupies a handful of slots for milliseconds each and comes nowhere near the cap. The limits are fixed rather than flags for that reason.
- An established SSH connection has a 10-minute idle timeout and, like the HTTP listener, deliberately no absolute deadline: an absolute one would cut a legitimate slow clone or a large push mid-transfer, while the idle one only reclaims a connection whose peer went away.
- SSH key registration proves possession of nothing (§8): a fingerprint belongs to whoever registers it first, and a duplicate is refused and audited rather than silently shared.
- The HTTP listener sets a header timeout and an idle timeout, and deliberately no read or write timeout: SSE streams and large packs are both long-lived by design.

On the server's own filesystem:

- The data dir is created 0700 and the database with its WAL siblings 0600. An existing wider data dir is tightened at startup, but only at the top level: the trees underneath - repos, hidden-test cache, logs, workspaces - are owner-only because each package creates them that way, which is also what covers git's own modes inside a repo.
- The intake hook socket is owner-only and refuses peers of any other uid, so a local account cannot feed the server submissions through it. The uid check is implemented on linux and darwin, the only released platforms; anywhere else it reports itself unsupported and the socket mode is the whole guard.

The web UI enforces role checks on every route; students can only read their own submissions.

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
- A per-check execution boundary between the solution and the hidden tests, so a course can keep them secret from a determined student (§14).
- SSH key proof of possession: signing a server challenge at registration, instead of first-come-first-served on the fingerprint.
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
