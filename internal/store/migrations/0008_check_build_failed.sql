-- A check can fail in its build phase, before the hidden tests leave the
-- workspace (SPEC §4.3). That phase's output is teacher-only, so the row
-- carries the fact rather than the text: the excerpt stays empty and the UI
-- renders a localized explanation.
ALTER TABLE check_results ADD COLUMN build_failed INTEGER NOT NULL DEFAULT 0;
