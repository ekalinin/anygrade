package web

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ekalinin/anygrade/internal/gradebook"
	"github.com/ekalinin/anygrade/internal/i18n"
	"github.com/ekalinin/anygrade/internal/intake"
	"github.com/ekalinin/anygrade/internal/runner"
	"github.com/ekalinin/anygrade/internal/store"
)

// paneCap matches the stored excerpt size (SPEC §13): the live view shows at
// most this much per check; the full log is a download away.
const paneCap int64 = runner.DefaultExcerptSize

const tailInterval = 400 * time.Millisecond

// tailState tracks one check's log file between polls.
type tailState struct {
	path   string
	offset int64
	capped bool
}

// submissionStream is the live view: log tails as `log<N>` events into
// pre-rendered panes, status flips as `status` events, and a terminal
// `done` event that makes the client fetch the authoritative fragment
// (which reconciles anything the best-effort hub dropped).
func (h *Handler) submissionStream(w http.ResponseWriter, r *http.Request) {
	sub, _, ok := h.loadSubmission(w, r)
	if !ok {
		return
	}
	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if terminalStatus(sub.Status) {
		sse.send("done", "")
		return
	}

	events, cancel := h.Hub.SubscribeSubmission(sub.ID)
	defer cancel()
	// Re-fetch once after subscribing: a terminal flip between the first
	// load and the subscription would otherwise leave the stream hanging.
	if cur, _, err := h.DB.GetSubmission(r.Context(), sub.ID); err == nil {
		if terminalStatus(cur.Status) {
			sse.send("done", "")
			return
		}
		sub = cur
	}

	// One tail per metadata check; panes with these indexes are already in
	// the DOM (no dynamic element creation, no OOB swaps).
	logDir := intake.SubmissionLogDir(h.DataDir, sub.ID)
	var tails []*tailState
	if task, _, ok := h.Course.Get().Task(sub.TaskID); ok {
		for _, c := range task.Checks {
			tails = append(tails, &tailState{path: filepath.Join(logDir, runner.LogFileName(c.Name))})
		}
	}

	lang := h.lang(r)
	ticker := time.NewTicker(tailInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			sse.send("status", statusBadge(lang, ev.Status))
			if terminalStatus(ev.Status) {
				h.drainTails(sse, lang, tails) // flush the last buffered output first
				sse.send("done", "")
				return
			}
		case <-ticker.C:
			h.drainTails(sse, lang, tails)
		}
	}
}

// drainTails emits new bytes of every growing log file, HTML-escaped, capped
// per pane at the excerpt size.
func (h *Handler) drainTails(sse *sseWriter, lang string, tails []*tailState) {
	for i, t := range tails {
		if t.capped {
			continue
		}
		chunk, err := readFrom(t.path, t.offset, paneCap-t.offset)
		if err != nil || len(chunk) == 0 {
			continue
		}
		t.offset += int64(len(chunk))
		payload := html.EscapeString(string(chunk))
		if t.offset >= paneCap {
			t.capped = true
			payload += "\n" + i18n.For(lang).T("stream.truncated")
		}
		sse.send(fmt.Sprintf("log%d", i), payload)
	}
}

// readFrom returns up to limit bytes of a file starting at offset; a missing
// file (check not started yet) reads as empty.
func readFrom(path string, offset, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= offset {
		return nil, err
	}
	n := min(st.Size()-offset, limit)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}

// statusBadge renders the status span swapped into the page header. The CSS
// class keeps the raw (English) status; only the visible label is localized.
func statusBadge(lang, status string) string {
	label := status
	switch status {
	case store.StatusInfraError:
		label = gradebook.StatusRetrying
	}
	return fmt.Sprintf(`<span class="badge st-%s">%s</span>`, label, i18n.For(lang).TStatus(label))
}
