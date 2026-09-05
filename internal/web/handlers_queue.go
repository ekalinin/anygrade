package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/store"
)

type queueData struct {
	CourseName string
	User       userView
	Rows       []queueRow
	// StreamIDs are the row ids this render put in the DOM, handed to the SSE
	// endpoint so it can reconcile them once the subscription is live.
	StreamIDs string
	Flash     string
}

// maxStreamIDs caps the reconciliation list: the ids come back from the client,
// and one query per id is work the server should not do on demand.
const maxStreamIDs = 500

// streamIDs renders the row ids for the stream URL.
func streamIDs(rows []queueRow) string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, strconv.FormatInt(row.Sub.ID, 10))
	}
	return strings.Join(ids, ",")
}

// parseStreamIDs reads the list back, ignoring anything unparseable.
func parseStreamIDs(raw string) []int64 {
	var ids []int64
	for part := range strings.SplitSeq(raw, ",") {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			continue
		}
		if ids = append(ids, id); len(ids) == maxStreamIDs {
			break
		}
	}
	return ids
}

type queueRow struct {
	Sub    store.Submission
	Login  string
	Status string // display status incl. canceled/retrying/error
}

// subDisplayStatus refines one submission's status for the queue view.
func subDisplayStatus(s store.Submission) string {
	if s.Status == store.StatusInfraError {
		switch {
		case s.CanceledAt != nil:
			return gradebook.StatusCanceled
		case s.RetryAt == nil:
			return gradebook.StatusError
		default:
			return gradebook.StatusRetrying
		}
	}
	return s.Status
}

// CanRecheck reports whether the row should offer the recheck button
// (SPEC §10: the queue view carries both cancel and recheck).
//
// Only the terminal `error` display status qualifies. `queued` and `running`
// have nothing to recheck yet and offer cancel instead; `retrying` re-runs by
// itself, so a manual recheck would only queue a competitor for it; `canceled`
// rows were cleared of `counts` by CancelSubmission, and TeacherRecheck picks
// the latest *counting* commit, so the button there would silently grade some
// other commit than the row shows - or fail outright when the canceled row was
// the student's only submission.
func (r queueRow) CanRecheck() bool {
	return r.Status == gradebook.StatusError
}

// loadQueueRows assembles the unfinished submissions with their owners'
// logins: the value the queue page renders and the API encodes. One ListUsers
// instead of a lookup per row - the table is course-sized, the queue is not
// necessarily.
func (h *Handler) loadQueueRows(ctx context.Context) ([]queueRow, error) {
	subs, err := h.DB.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	users, err := h.DB.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	logins := map[int64]string{}
	for _, x := range users {
		logins[x.ID] = x.Login
	}
	rows := make([]queueRow, len(subs))
	for i, s := range subs {
		rows[i] = queueRow{Sub: s, Login: logins[s.UserID], Status: subDisplayStatus(s)}
	}
	return rows, nil
}

func (h *Handler) queuePage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	rows, err := h.loadQueueRows(r.Context())
	if err != nil {
		h.httpError(w, r, "error.load_failed", http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, "queue", queueData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       h.userViewOf(u),
		Rows:       rows,
		StreamIDs:  streamIDs(rows),
		Flash:      r.URL.Query().Get("flash"),
	})
}

// queueStream re-renders one queue row per event ("sub-<id>"); terminal rows
// render with their final status and stop changing.
func (h *Handler) queueStream(w http.ResponseWriter, r *http.Request) {
	sse, ok := newSSEWriter(w)
	if !ok {
		h.httpError(w, r, "error.streaming_unsupported", http.StatusInternalServerError)
		return
	}
	events, cancel := h.Hub.SubscribeAll()
	defer cancel()
	lang := h.lang(r)
	// Reconcile the rows the page rendered before this subscription existed: a
	// state change in that gap is not replayed, and the Hub may drop on
	// overflow, so without this a row can sit at "running" until the next
	// event or a manual reload.
	for _, id := range parseStreamIDs(r.URL.Query().Get("ids")) {
		h.sendQueueRow(r, sse, lang, id)
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			h.sendQueueRow(r, sse, lang, ev.SubID)
		}
	}
}

// sendQueueRow re-renders one queue row from the authoritative state.
func (h *Handler) sendQueueRow(r *http.Request, sse *sseWriter, lang string, id int64) {
	sub, _, err := h.DB.GetSubmission(r.Context(), id)
	if err != nil {
		return
	}
	target, err := h.DB.GetUserByID(r.Context(), sub.UserID)
	if err != nil {
		return
	}
	row := queueRow{Sub: sub, Login: target.Login, Status: subDisplayStatus(sub)}
	html, err := renderPartial(lang, "queue-row", row)
	if err != nil {
		return
	}
	sse.send(fmt.Sprintf("sub-%d", sub.ID), html)
}

func (h *Handler) cancelSubmission(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sub, _, err := h.DB.GetSubmission(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ok, err := h.Cancel.Cancel(r.Context(), id)
	if err != nil {
		h.httpError(w, r, "error.cancel_failed", http.StatusInternalServerError)
		return
	}
	if ok {
		target, terr := h.DB.GetUserByID(r.Context(), sub.UserID)
		if terr == nil {
			_ = h.DB.Log(r.Context(), store.Event{
				ActorID: &actor.ID, Kind: "submission.cancel",
				Target: target.Login + "/" + sub.TaskID,
				Detail: fmt.Sprintf("#%d", id),
			})
		}
	}
	http.Redirect(w, r, "/queue", http.StatusSeeOther)
}

// recheckSubmission re-grades the (student, task) pair of one queue row on the
// teacher's behalf, the same action the student page exposes - so the audit
// event is the one TeacherRecheck itself writes, not a second one here.
//
// The route is deliberately not gated on the row's status: CanRecheck only
// decides where the button is drawn, and the very same recheck is reachable for
// any pair from /students/{login}. TeacherRecheck re-grades the latest counting
// commit, which for an `error` row is normally that row's own commit but is a
// newer one when the student has pushed since - the redirect lands on the new
// submission, where the commit is spelled out.
func (h *Handler) recheckSubmission(w http.ResponseWriter, r *http.Request) {
	actor := user(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sub, _, err := h.DB.GetSubmission(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fresh, warn, err := h.Recheck.TeacherRecheck(r.Context(), actor, sub.UserID, sub.TaskID)
	switch {
	case errors.Is(err, intake.ErrNothingToRecheck):
		http.Redirect(w, r, "/queue?flash=nothing_to_recheck", http.StatusSeeOther)
	case err != nil:
		h.httpError(w, r, "error.recheck_failed", http.StatusInternalServerError)
	default:
		http.Redirect(w, r, submissionURL(fresh.ID, warn), http.StatusSeeOther)
	}
}

// submissionURL points at a freshly queued submission, carrying a recheck
// warning along as a flash code. RecheckWarning values are package constants
// of intake, so they need no query escaping.
func submissionURL(id int64, warn intake.RecheckWarning) string {
	u := fmt.Sprintf("/submissions/%d", id)
	if warn != "" {
		u += "?flash=" + string(warn)
	}
	return u
}
