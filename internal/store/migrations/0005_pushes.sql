-- The push log (SPEC §13: "rapid successive pushes ... each becomes a
-- submission ... they run in order").
--
-- Before this table the only record of an accepted push was git refs, and a ref
-- holds none of what the requirement needs: not the push boundaries, not the
-- time it arrived, not an identity (a force-push cycle X -> A -> X repeats the
-- same pair of ends), and it cannot be moved in the same transaction that
-- records a submission. A row can carry all four.
--
-- processed_at NULL means the push still has to be graded: hook handlers are
-- concurrent and can die, so whoever holds the student's lock next drains
-- every pending row of that student in id order.

CREATE TABLE pushes (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  ref          TEXT    NOT NULL,
  old_sha      TEXT    NOT NULL,
  new_sha      TEXT    NOT NULL,
  received_at  TEXT    NOT NULL,
  processed_at TEXT
);
CREATE INDEX idx_pushes_pending ON pushes(user_id, processed_at, id);
