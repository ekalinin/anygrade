package web

import (
	"net/http"
	"strconv"

	"github.com/ekalinin/anygrade/internal/store"
)

// auditPageSize is the number of events shown per page.
const auditPageSize = 50

type auditData struct {
	CourseName string
	User       userView
	Events     []store.EventRow
	Kinds      []string
	Kind       string
	Target     string
	Page       int
	PrevPage   int
	NextPage   int
	HasPrev    bool
	HasNext    bool
}

// auditPage is the teacher-only global audit log (SPEC §10).
func (h *Handler) auditPage(w http.ResponseWriter, r *http.Request) {
	u := user(r)
	kind := r.URL.Query().Get("kind")
	target := r.URL.Query().Get("target")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 0 {
		page = 0
	}

	// Fetch one extra row to know whether a next page exists.
	events, err := h.DB.ListEvents(r.Context(), kind, target, auditPageSize+1, page*auditPageSize)
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	hasNext := len(events) > auditPageSize
	if hasNext {
		events = events[:auditPageSize]
	}
	kinds, err := h.DB.ListEventKinds(r.Context())
	if err != nil {
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}

	h.renderPage(w, r, "audit", auditData{
		CourseName: h.Course.Get().Resolved.Course.Name,
		User:       userView{u.Login, u.DisplayName, u.Role},
		Events:     events,
		Kinds:      kinds,
		Kind:       kind,
		Target:     target,
		Page:       page,
		PrevPage:   page - 1,
		NextPage:   page + 1,
		HasPrev:    page > 0,
		HasNext:    hasNext,
	})
}
