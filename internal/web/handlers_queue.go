package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/store"
)

type queueData struct {
	CourseName string
	User       userView
	Rows       []queueRow
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

func (h *Handler) queuePage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	subs, err := h.DB.ListActive(r.Context())
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	users, err := h.DB.ListUsers(r.Context())
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	logins := map[int64]string{}
	for _, x := range users {
		logins[x.ID] = x.Login
	}
	rows := make([]queueRow, len(subs))
	for i, s := range subs {
		rows[i] = queueRow{Sub: s, Login: logins[s.UserID], Status: subDisplayStatus(s)}
	}
	h.renderPage(w, r, "queue", queueData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       userView{u.Login, u.DisplayName, u.Role},
		Rows:       rows,
	})
}

// queueStream re-renders one queue row per event ("sub-<id>"); terminal rows
// render with their final status and stop changing.
func (h *Handler) queueStream(w http.ResponseWriter, r *http.Request) {
	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	events, cancel := h.Hub.SubscribeAll()
	defer cancel()
	lang := h.lang(r)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			sub, _, err := h.DB.GetSubmission(r.Context(), ev.SubID)
			if err != nil {
				continue
			}
			target, err := h.DB.GetUserByID(r.Context(), sub.UserID)
			if err != nil {
				continue
			}
			row := queueRow{Sub: sub, Login: target.Login, Status: subDisplayStatus(sub)}
			html, err := renderPartial(lang, "queue-row", row)
			if err != nil {
				continue
			}
			sse.send(fmt.Sprintf("sub-%d", sub.ID), html)
		}
	}
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
		http.Error(w, "cancel failed", http.StatusInternalServerError)
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
