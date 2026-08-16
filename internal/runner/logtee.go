package runner

import (
	"io"
	"os"

	"github.com/ekalinin/anygrade/internal/config"
)

// DefaultExcerptSize is the excerpt size used when a Job carries none; the
// full log always lives on disk (SPEC §13). Courses set their own through
// `runner.log_excerpt`, which resolves to Job.Spec.LogExcerpt.
const DefaultExcerptSize = config.DefaultLogExcerpt

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

// checkLog bundles a check's log file with its in-memory excerpt tail and an
// optional live mirror.
type checkLog struct {
	file *os.File
	tail *tailBuffer
	w    io.Writer
}

// openCheckLog creates the check's log file and its excerpt tail. A
// non-positive excerpt falls back to DefaultExcerptSize, so a Job assembled
// without a resolved config still behaves.
func openCheckLog(path string, mirror io.Writer, excerpt int64) (*checkLog, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if excerpt <= 0 {
		excerpt = DefaultExcerptSize
	}
	tail := newTailBuffer(int(excerpt))
	var w io.Writer = io.MultiWriter(f, tail)
	if mirror != nil {
		w = io.MultiWriter(f, tail, mirror)
	}
	return &checkLog{file: f, tail: tail, w: w}, nil
}

func (l *checkLog) Write(p []byte) (int, error) { return l.w.Write(p) }
func (l *checkLog) Excerpt() string             { return l.tail.String() }
func (l *checkLog) Close() error                { return l.file.Close() }
