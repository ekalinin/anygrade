package gitserver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// errOversizePush stops the copy into receive-pack once a push has gone past
// course.yaml's `limits.max_push_size` (SPEC §13).
var errOversizePush = errors.New("push exceeds max_push_size")

// oversizeReader is the transport-side push cap. receive.maxInputSize would
// stop the same push, but git's own report ("fatal: pack exceeds maximum
// allowed size", and over HTTP not even that - only "unpack-objects abnormal
// exit") says neither what the limit is nor what to do about it. Failing the
// read ourselves puts us back in charge of the message.
type oversizeReader struct {
	r    io.Reader
	left int64 // bytes still allowed, limit+1 at the start
	hit  bool
}

// newOversizeReader caps r at limit bytes. The budget is limit+1 so that a
// push of exactly limit bytes still reaches EOF through the underlying reader.
func newOversizeReader(r io.Reader, limit int64) *oversizeReader {
	return &oversizeReader{r: r, left: limit + 1}
}

func (o *oversizeReader) Read(p []byte) (int, error) {
	if o.left <= 0 {
		o.hit = true
		return 0, errOversizePush
	}
	if int64(len(p)) > o.left {
		p = p[:o.left]
	}
	n, err := o.r.Read(p)
	o.left -= int64(n)
	return n, err
}

// heldWriterCap bounds how much of receive-pack's output is held back.
const heldWriterCap = 1 << 20

// heldWriter keeps receive-pack's answer in memory until the push is known to
// fit, so an oversized one can be answered with our report instead of git's
// "unpack-objects abnormal exit". Holding it costs no streaming: everything a
// push produces - the report-status and the hook's push feedback - is a few
// kilobytes, which net/http buffers anyway. Past the cap it gives up and
// streams, which only happens for a push that already got far enough to be
// graded, i.e. one that was never oversized.
type heldWriter struct {
	w         io.Writer
	buf       bytes.Buffer
	streaming bool
}

func (h *heldWriter) Write(p []byte) (int, error) {
	if h.streaming {
		return h.w.Write(p)
	}
	h.buf.Write(p)
	if h.buf.Len() > heldWriterCap {
		h.streaming = true
		if err := h.flush(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flush passes on whatever is still held.
func (h *heldWriter) flush() error {
	if h.buf.Len() == 0 {
		return nil
	}
	_, err := h.w.Write(h.buf.Bytes())
	h.buf.Reset()
	return err
}

// held reports whether nothing has reached the client yet.
func (h *heldWriter) held() bool { return !h.streaming }

// writeOversizeReport answers a killed receive-pack with a receive-pack result
// of our own: a side-band-2 line, which git prints verbatim as `remote: ...`,
// and a side-band-1 report-status whose unpack error git prints as `error:
// remote unpack failed: <msg>`. The ref advertisement comes from git itself,
// so side-band-64k is always on the wire.
func writeOversizeReport(w io.Writer, limit int64) {
	report := pktLine("unpack anygrade: push exceeds max_push_size\n") + "0000"
	_, _ = io.WriteString(w, pktLine("\x02anygrade: push rejected: "+oversizeMessage(limit)+"\n"))
	_, _ = io.WriteString(w, pktLine("\x01"+report))
	_, _ = io.WriteString(w, "0000")
}

// oversizeMessage is the single wording the student sees on either transport.
func oversizeMessage(limit int64) string {
	return fmt.Sprintf("it is larger than max_push_size (%s); "+
		"drop the large files from the commit (git rm --cached, then amend) and push again",
		humanSize(limit))
}

// pktLine frames one git pkt-line.
func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

// humanSize renders a byte count the way course.yaml spells it.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
