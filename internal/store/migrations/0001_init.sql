CREATE TABLE users (
  id           INTEGER PRIMARY KEY,
  login        TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL DEFAULT '',
  role         TEXT NOT NULL CHECK (role IN ('student','teacher')),
  state        TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','disabled')),
  created_at   TEXT NOT NULL
);

CREATE TABLE tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hash         TEXT NOT NULL UNIQUE,
  created_at   TEXT NOT NULL,
  last_used_at TEXT
);
CREATE INDEX idx_tokens_user ON tokens(user_id);

CREATE TABLE ssh_keys (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  fingerprint TEXT NOT NULL UNIQUE,
  public_key  TEXT NOT NULL,
  created_at  TEXT NOT NULL
);

CREATE TABLE invites (
  id         INTEGER PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL,
  used_at    TEXT
);

CREATE TABLE submissions (
  id              INTEGER PRIMARY KEY,
  user_id         INTEGER NOT NULL REFERENCES users(id),
  task_id         TEXT    NOT NULL,
  commit_sha      TEXT    NOT NULL,
  received_at     TEXT    NOT NULL,
  attempt_no      INTEGER,
  counts          INTEGER NOT NULL DEFAULT 1,
  status          TEXT    NOT NULL CHECK (status IN
                    ('queued','running','done','infra_error',
                     'rejected_deadline','rejected_limit')),
  raw_score       REAL,
  penalty_percent REAL,
  final_score     REAL,
  log_dir         TEXT    NOT NULL DEFAULT '',
  worker_note     TEXT    NOT NULL DEFAULT '',
  retries         INTEGER NOT NULL DEFAULT 0,
  retry_at        TEXT,
  started_at      TEXT
);
CREATE INDEX idx_sub_claim ON submissions(status, retry_at);
CREATE INDEX idx_sub_user_task ON submissions(user_id, task_id);
CREATE INDEX idx_sub_task ON submissions(task_id);

CREATE TABLE check_results (
  id            INTEGER PRIMARY KEY,
  submission_id INTEGER NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
  name          TEXT    NOT NULL,
  passed        INTEGER NOT NULL,
  exit_code     INTEGER NOT NULL,
  duration_ms   INTEGER NOT NULL,
  weight        INTEGER NOT NULL,
  skipped       INTEGER NOT NULL DEFAULT 0,
  timed_out     INTEGER NOT NULL DEFAULT 0,
  log_excerpt   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_check_sub ON check_results(submission_id);

CREATE TABLE score_overrides (
  user_id    INTEGER NOT NULL REFERENCES users(id),
  task_id    TEXT    NOT NULL,
  score      REAL    NOT NULL,
  comment    TEXT    NOT NULL DEFAULT '',
  teacher_id INTEGER NOT NULL REFERENCES users(id),
  created_at TEXT    NOT NULL,
  PRIMARY KEY (user_id, task_id)
);

CREATE TABLE events (
  id         INTEGER PRIMARY KEY,
  actor_id   INTEGER REFERENCES users(id),
  kind       TEXT    NOT NULL,
  target     TEXT    NOT NULL DEFAULT '',
  detail     TEXT    NOT NULL DEFAULT '',
  created_at TEXT    NOT NULL
);
CREATE INDEX idx_events_created ON events(created_at);
