-- worker_note has two audiences and only one of them may read every word of
-- it: a docker failure names the image and quotes the daemon, a prepare
-- failure quotes a path inside the data dir, while the scrubbed hidden-tests
-- message is written for the student who has to act on it (SPEC §14). The row
-- therefore carries the student-safe projection of the note separately;
-- worker_note keeps the operator's full text for the teacher queue view.
ALTER TABLE submissions ADD COLUMN student_note TEXT NOT NULL DEFAULT '';

-- Backfill only what is student-safe by construction: reject reasons, tamper
-- notes and the cancel note, all written by paths that produce nothing else.
-- A legacy infra_error row carries an unclassified worker note, so it keeps an
-- empty student note and reads to its owner exactly as it does today.
UPDATE submissions SET student_note = worker_note
WHERE status <> 'infra_error' OR canceled_at IS NOT NULL;
