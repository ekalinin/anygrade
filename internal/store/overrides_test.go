package store

import "testing"

// TestScoreOverrideLifecycle: set, get, upsert, and delete.
func TestScoreOverrideLifecycle(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	teacher, err := db.CreateUser(t.Context(), "prof", "Prof", "teacher")
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := db.GetScoreOverride(t.Context(), u.ID, "t1"); err != nil || ok {
		t.Fatalf("get on empty: ok=%v err=%v", ok, err)
	}

	if err := db.SetScoreOverride(t.Context(), ScoreOverride{
		UserID: u.ID, TaskID: "t1", Score: 80, Comment: "fixed manually", TeacherID: teacher.ID,
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.GetScoreOverride(t.Context(), u.ID, "t1")
	if err != nil || !ok {
		t.Fatalf("get after set: ok=%v err=%v", ok, err)
	}
	if got.Score != 80 || got.Comment != "fixed manually" || got.TeacherID != teacher.ID {
		t.Errorf("override: %+v", got)
	}

	// Upsert: setting again replaces score and comment, still one row.
	if err := db.SetScoreOverride(t.Context(), ScoreOverride{
		UserID: u.ID, TaskID: "t1", Score: 95, Comment: "revised", TeacherID: teacher.ID,
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err = db.GetScoreOverride(t.Context(), u.ID, "t1")
	if err != nil || !ok {
		t.Fatalf("get after upsert: ok=%v err=%v", ok, err)
	}
	if got.Score != 95 || got.Comment != "revised" {
		t.Errorf("override after upsert: %+v", got)
	}
	all, err := db.ListScoreOverrides(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 row after upsert, got %d", len(all))
	}

	if err := db.DeleteScoreOverride(t.Context(), u.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.GetScoreOverride(t.Context(), u.ID, "t1"); err != nil || ok {
		t.Fatalf("get after delete: ok=%v err=%v", ok, err)
	}

	// Deleting a missing override is a no-op.
	if err := db.DeleteScoreOverride(t.Context(), u.ID, "t1"); err != nil {
		t.Fatalf("delete missing override: %v", err)
	}
}

// TestListScoreOverrides: results are ordered by user then task.
func TestListScoreOverrides(t *testing.T) {
	db := openTestDB(t)
	u := testUser(t, db)
	teacher, err := db.CreateUser(t.Context(), "prof", "Prof", "teacher")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SetScoreOverride(t.Context(), ScoreOverride{
		UserID: u.ID, TaskID: "taskB", Score: 70, TeacherID: teacher.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetScoreOverride(t.Context(), ScoreOverride{
		UserID: u.ID, TaskID: "taskA", Score: 60, TeacherID: teacher.ID,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListScoreOverrides(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].TaskID != "taskA" || all[1].TaskID != "taskB" {
		t.Fatalf("overrides: %+v", all)
	}

	other, err := db.CreateUser(t.Context(), "student2", "Student Two", "student")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetScoreOverride(t.Context(), ScoreOverride{
		UserID: other.ID, TaskID: "taskA", Score: 50, TeacherID: teacher.ID,
	}); err != nil {
		t.Fatal(err)
	}

	all, err = db.ListScoreOverrides(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 overrides, got %d", len(all))
	}
}
