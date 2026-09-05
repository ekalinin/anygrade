-- Per-test-case results of a check that declares a `parser:` (SPEC §4.3, §12).
-- A table of its own because a check_results row is the outcome of one whole
-- command and has no room for a list; the cascade ties the rows to the check
-- they detail, so re-finishing a submission replaces them with its own.
CREATE TABLE check_cases (
  id              INTEGER PRIMARY KEY,
  check_result_id INTEGER NOT NULL REFERENCES check_results(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  status          TEXT    NOT NULL CHECK (status IN ('passed','failed','skipped')),
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  message         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_check_cases ON check_cases(check_result_id);

-- A parser that could not read its report leaves the check scored by its exit
-- code, exactly as a course with no parser is. The row carries the fact rather
-- than the text, like build_failed: the UI renders a localized explanation and
-- the log excerpt stays what the student reads.
ALTER TABLE check_results ADD COLUMN parse_failed INTEGER NOT NULL DEFAULT 0;
