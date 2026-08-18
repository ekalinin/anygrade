package runner

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ekalinin/anygrade/internal/config"
)

// DefaultExcerptSize is the excerpt size used when a Job carries none; the
// full log always lives on disk (SPEC §13). Courses set their own through
// `runner.log_excerpt`, which resolves to Job.Spec.LogExcerpt.
const DefaultExcerptSize = config.DefaultLogExcerpt

// DefaultLogMax is the on-disk cap used when a Job carries none
// (`runner.log_max`, Job.Spec.LogMax).
const DefaultLogMax = config.DefaultLogMax

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func newTailBuffer(size int) *tailBuffer { return &tailBuffer{max: size} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		// Copy so the backing array does not grow unboundedly.
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if t.truncated {
		return "[...truncated...]\n" + string(t.buf)
	}
	return string(t.buf)
}

// capWriter writes check output to the log file until max bytes, then stops.
// Check output is untrusted: without a cap a student process that prints until
// the timeout fills the host disk, taking the database down with it.
//
// Write never reports an error. A failed log write - ENOSPC above all - must
// not cost the check its result, so the file is dropped, the first error is
// remembered for the excerpt and logged once for the operator, and the check
// runs to completion with its output still going to the tail and the mirror.
type capWriter struct {
	file    *os.File
	name    string // check name, for the operator-facing log line
	max     int64
	written int64
	capped  bool
	err     error
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.file == nil {
		return n, nil
	}
	chunk := p
	if room := w.max - w.written; int64(len(chunk)) > room {
		chunk, w.capped = chunk[:room], true
	}
	if len(chunk) > 0 {
		nw, err := w.file.Write(chunk)
		w.written += int64(nw)
		if err != nil {
			w.fail(err)
			return n, nil
		}
	}
	if w.capped {
		// Best effort: the marker is what the reader of the file needs, and the
		// cap is already enforced whether or not it lands.
		fmt.Fprintf(w.file, "\n%s\n", w.capNote())
		w.file = nil
	}
	return n, nil
}

func (w *capWriter) fail(err error) {
	w.err = err
	w.file = nil
	slog.Warn("check log write failed, log dropped", "check", w.name, "err", err)
}

func (w *capWriter) capNote() string {
	return fmt.Sprintf("anygrade: log truncated at %d bytes (runner.log_max)", w.max)
}

// note is appended to the excerpt so the DB and the UI carry the same verdict
// as the file. It stays free of host paths: students read the excerpt.
func (w *capWriter) note() string {
	switch {
	case w.err != nil:
		return "\nanygrade: the full log could not be written\n"
	case w.capped:
		return "\n" + w.capNote() + "\n"
	}
	return ""
}

// checkLog bundles a check's log file with its in-memory excerpt tail and an
// optional live mirror.
type checkLog struct {
	file *os.File
	cap  *capWriter
	tail *tailBuffer
	w    io.Writer
}

// openCheckLog creates the check's log file, its excerpt tail and its on-disk
// cap. Non-positive sizes fall back to the built-in defaults, so a Job
// assembled without a resolved config still behaves.
func openCheckLog(path, name string, mirror io.Writer, excerpt, logMax int64) (*checkLog, error) {
	// 0600, not os.Create's 0666: a check log is student output the teacher
	// reads through the UI, not something for every account on the host.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if excerpt <= 0 {
		excerpt = DefaultExcerptSize
	}
	if logMax <= 0 {
		logMax = DefaultLogMax
	}
	tail := newTailBuffer(int(excerpt))
	capped := &capWriter{file: f, name: name, max: logMax}
	writers := []io.Writer{capped, tail}
	if mirror != nil {
		writers = append(writers, mirror)
	}
	return &checkLog{file: f, cap: capped, tail: tail, w: io.MultiWriter(writers...)}, nil
}

func (l *checkLog) Write(p []byte) (int, error) { return l.w.Write(p) }
func (l *checkLog) Excerpt() string             { return l.tail.String() + l.cap.note() }
func (l *checkLog) Close() error                { return l.file.Close() }
