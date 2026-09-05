package web

import (
	"net/http"
	"time"
)

// The API endpoints (SPEC §10.2). Each one loads exactly what its page loads -
// the same read model, through the same functions - and maps it onto an
// explicit DTO. The DTOs are written out rather than tags bolted onto the read
// model, because the field set is the contract: renaming a field inside the
// read model must stay invisible to clients, and a field must not reach the
// wire merely because somebody added it to a page.
//
// Read-only by design in v1. The write actions (recheck, override, cancel) are
// forms today, and what protects them is the same-origin check on POST - which
// says nothing about a client that composes its own headers. Writes therefore
// need a CSRF story of their own, and that is a separate decision.

// apiUserDTO is the caller's identity, from the same userView every page header
// carries.
type apiUserDTO struct {
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func (h *Handler) apiMe(w http.ResponseWriter, r *http.Request) {
	v := h.userViewOf(user(r))
	writeJSON(w, http.StatusOK, apiUserDTO{Login: v.Login, DisplayName: v.DisplayName, Role: v.Role})
}

type apiTasksDTO struct {
	Course string       `json:"course"`
	Tasks  []apiTaskDTO `json:"tasks"`
}

// apiTaskDTO is one dashboard row. Score is what the page prints - an override
// wins over the computed one (SPEC §9) - and ComputedScore is what the machine
// arrived at, so a script sees the same correction the student does instead of
// two sources disagreeing.
type apiTaskDTO struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	MaxScore           int             `json:"max_score"`
	Status             string          `json:"status"`
	Score              *float64        `json:"score"`
	ComputedScore      *float64        `json:"computed_score"`
	Override           *apiOverrideDTO `json:"override"`
	Attempts           int             `json:"attempts"`
	LatestSubmissionID *int64          `json:"latest_submission_id"`
	SoftDeadline       *time.Time      `json:"soft_deadline"`
	HardDeadline       *time.Time      `json:"hard_deadline"`
}

type apiOverrideDTO struct {
	Score   float64 `json:"score"`
	Comment string  `json:"comment"`
}

// apiTasks is the caller's own dashboard: their tasks, statuses and scores.
func (h *Handler) apiTasks(w http.ResponseWriter, r *http.Request) {
	course := h.Course.Get()
	views, err := h.loadDashboard(r.Context(), course, user(r).ID)
	if err != nil {
		apiFail(w, http.StatusInternalServerError, codeInternal, "load failed")
		return
	}
	tasks := make([]apiTaskDTO, 0, len(views))
	for _, v := range views {
		t := apiTaskDTO{
			ID: v.Task.ID, Name: v.Task.Name, MaxScore: v.Task.Score,
			Status: v.Status, Score: v.Display(), ComputedScore: v.Score,
			Attempts:     v.Attempts,
			SoftDeadline: v.Task.Deadline.Soft, HardDeadline: v.Task.Deadline.Hard,
		}
		if v.Override != nil {
			t.Override = &apiOverrideDTO{Score: v.Override.Score, Comment: v.Override.Comment}
		}
		if v.Latest != nil {
			t.LatestSubmissionID = &v.Latest.ID
		}
		tasks = append(tasks, t)
	}
	writeJSON(w, http.StatusOK, apiTasksDTO{Course: course.Resolved.Course.Name, Tasks: tasks})
}

// apiSubmissionDTO is one submission with its check results. There is
// deliberately no way to reach a full check log or a build-phase log from here:
// both are teacher-only in the UI (SPEC §14) and the API is not the way around
// that, so what a check carries is the stored excerpt the page already shows.
type apiSubmissionDTO struct {
	ID             int64         `json:"id"`
	TaskID         string        `json:"task_id"`
	TaskName       string        `json:"task_name"`
	MaxScore       int           `json:"max_score"`
	Commit         string        `json:"commit"`
	Status         string        `json:"status"`
	Running        bool          `json:"running"`
	Rejected       bool          `json:"rejected"`
	AttemptNo      *int          `json:"attempt_no"`
	Counts         bool          `json:"counts"`
	ReceivedAt     time.Time     `json:"received_at"`
	StartedAt      *time.Time    `json:"started_at"`
	RawScore       *float64      `json:"raw_score"`
	PenaltyPercent *float64      `json:"penalty_percent"`
	FinalScore     *float64      `json:"final_score"`
	Note           string        `json:"note"`
	Checks         []apiCheckDTO `json:"checks"`
}

type apiCheckDTO struct {
	Name        string `json:"name"`
	Passed      bool   `json:"passed"`
	ExitCode    int    `json:"exit_code"`
	DurationMS  int64  `json:"duration_ms"`
	Weight      int    `json:"weight"`
	Skipped     bool   `json:"skipped"`
	TimedOut    bool   `json:"timed_out"`
	BuildFailed bool   `json:"build_failed"`
	LogExcerpt  string `json:"log_excerpt"`
}

// apiSubmission serves one submission: the caller's own, or any for a teacher.
// Both the ownership gate and the viewer-dependent projection are the page's -
// findSubmission decides who may read the row, and submissionData decides which
// note this viewer gets (the student-safe one, or the operator's).
func (h *Handler) apiSubmission(w http.ResponseWriter, r *http.Request) {
	sub, checks, ok := h.findSubmission(r)
	if !ok {
		apiNotFound(w)
		return
	}
	data := h.submissionData(sub, checks, user(r))
	dto := apiSubmissionDTO{
		ID: sub.ID, TaskID: sub.TaskID, TaskName: data.TaskName, MaxScore: data.TaskScore,
		Commit: sub.CommitSHA, Status: data.Status,
		Running: data.Running, Rejected: data.Rejected,
		AttemptNo: sub.AttemptNo, Counts: sub.Counts,
		ReceivedAt: sub.ReceivedAt, StartedAt: sub.StartedAt,
		RawScore: sub.RawScore, PenaltyPercent: sub.PenaltyPercent, FinalScore: sub.FinalScore,
		Note:   data.Note,
		Checks: make([]apiCheckDTO, 0, len(data.Checks)),
	}
	for _, c := range data.Checks {
		dto.Checks = append(dto.Checks, apiCheckDTO{
			Name: c.Name, Passed: c.Passed, ExitCode: c.ExitCode,
			DurationMS: c.Duration.Milliseconds(), Weight: c.Weight,
			Skipped: c.Skipped, TimedOut: c.TimedOut, BuildFailed: c.BuildFailed,
			LogExcerpt: c.LogExcerpt,
		})
	}
	writeJSON(w, http.StatusOK, dto)
}

type apiMatrixDTO struct {
	Tasks []apiMatrixTaskDTO `json:"tasks"`
	Rows  []apiMatrixRowDTO  `json:"rows"`
}

type apiMatrixTaskDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MaxScore int    `json:"max_score"`
}

type apiMatrixRowDTO struct {
	Login       string                `json:"login"`
	DisplayName string                `json:"display_name"`
	Total       float64               `json:"total"`
	Cells       map[string]apiCellDTO `json:"cells"`
}

type apiCellDTO struct {
	Status             string   `json:"status"`
	Score              float64  `json:"score"`
	ComputedScore      *float64 `json:"computed_score"`
	OverrideScore      *float64 `json:"override_score"`
	LatestSubmissionID *int64   `json:"latest_submission_id"`
}

// apiMatrix is the teacher's whole gradebook, the same value the matrix page
// and the CSV export render. Unfiltered on purpose: the page's q/task/status
// filters are for a human scanning a screen, and a script that has the board
// can select from it itself.
func (h *Handler) apiMatrix(w http.ResponseWriter, r *http.Request) {
	m, err := h.buildMatrix(r)
	if err != nil {
		apiFail(w, http.StatusInternalServerError, codeInternal, "load failed")
		return
	}
	dto := apiMatrixDTO{
		Tasks: make([]apiMatrixTaskDTO, 0, len(m.Tasks)),
		Rows:  make([]apiMatrixRowDTO, 0, len(m.Rows)),
	}
	for _, t := range m.Tasks {
		dto.Tasks = append(dto.Tasks, apiMatrixTaskDTO{ID: t.ID, Name: t.Name, MaxScore: t.MaxScore})
	}
	for _, row := range m.Rows {
		out := apiMatrixRowDTO{
			Login: row.User.Login, DisplayName: row.User.DisplayName, Total: row.Total,
			Cells: make(map[string]apiCellDTO, len(m.Tasks)),
		}
		for _, t := range m.Tasks {
			cell := row.Cells[t.ID]
			out.Cells[t.ID] = apiCellDTO{
				// cellStatus, not Cell.Status: Build blanks an untouched cell so
				// the page can draw a dash, and a blank status is not a value a
				// client can branch on.
				Status:             cellStatus(row, t.ID),
				Score:              cell.Display,
				ComputedScore:      cell.Computed,
				OverrideScore:      cell.Override,
				LatestSubmissionID: apiSubID(cell.LatestSubID),
			}
		}
		dto.Rows = append(dto.Rows, out)
	}
	writeJSON(w, http.StatusOK, dto)
}

type apiQueueDTO struct {
	Rows []apiQueueRowDTO `json:"rows"`
}

// apiQueueRowDTO is one unfinished submission. Note is the worker's own, whole:
// the queue is teacher-only, and the note is why an infra_error is sitting
// there.
type apiQueueRowDTO struct {
	ID         int64      `json:"id"`
	Login      string     `json:"login"`
	TaskID     string     `json:"task_id"`
	Status     string     `json:"status"`
	ReceivedAt time.Time  `json:"received_at"`
	StartedAt  *time.Time `json:"started_at"`
	AttemptNo  *int       `json:"attempt_no"`
	Retries    int        `json:"retries"`
	Note       string     `json:"note"`
}

// apiQueue is the teacher's queue view: queued, running and infra_error rows.
func (h *Handler) apiQueue(w http.ResponseWriter, r *http.Request) {
	rows, err := h.loadQueueRows(r.Context())
	if err != nil {
		apiFail(w, http.StatusInternalServerError, codeInternal, "load failed")
		return
	}
	dto := apiQueueDTO{Rows: make([]apiQueueRowDTO, 0, len(rows))}
	for _, row := range rows {
		dto.Rows = append(dto.Rows, apiQueueRowDTO{
			ID: row.Sub.ID, Login: row.Login, TaskID: row.Sub.TaskID, Status: row.Status,
			ReceivedAt: row.Sub.ReceivedAt, StartedAt: row.Sub.StartedAt,
			AttemptNo: row.Sub.AttemptNo, Retries: row.Sub.Retries,
			Note: row.Sub.WorkerNote,
		})
	}
	writeJSON(w, http.StatusOK, dto)
}

// apiSubID renders the read model's "no submission" sentinel as null: 0 is what
// gradebook uses for it, and 0 looks like an id to a client.
func apiSubID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}
