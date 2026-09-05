package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/ekalinin/anygrade/internal/config"
	"github.com/ekalinin/anygrade/internal/testreport"
)

// attachCases fills in the per-test-case results of a check that declares a
// `parser:` (SPEC §4.3).
//
// It cannot fail the check, and that is the whole contract: a report anygrade
// could not read is the parser's problem, never the student's, so every
// failure lands on ParseFailed and the check keeps exactly the verdict its
// exit code gave it - which is what a course without parsers gets anyway.
//
// A timed-out check is not parsed at all. It was killed mid-run, so whatever
// it printed is the beginning of a report rather than one, and crediting a
// hanging solution for the cases it managed before the clock ran out is not
// what a timeout means (SPEC §13).
func attachCases(ctx context.Context, job Job, c config.Check, o *Outcome, ex checkExecutor) {
	if !testreport.Enabled(c.Parser) || o.Skipped || o.BuildFailed || o.TimedOut {
		return
	}
	cases, err := parseReport(ctx, job, c, o.LogPath, ex)
	if err == nil {
		o.Cases = cases
		return
	}
	o.ParseFailed = true
	// Operator detail only: the submission page already tells both audiences
	// that the check was scored by its exit code, and this line is reachable
	// from student output, so it must not be able to fill a log on its own.
	slog.Debug("per-test-case report could not be read",
		"check", c.Name, "parser", c.Parser, "err", err)
}

// parseReport reads what the parser parses: the check's own log - stdout and
// stderr as the run produced them, which is where `go test -json` and TAP put
// the report - or the file the check wrote when `parser_file:` names one, which
// is how a format that is a file by convention (JUnit XML) reaches us.
func parseReport(ctx context.Context, job Job, c config.Check, logPath string, ex checkExecutor) ([]testreport.Case, error) {
	var (
		data []byte
		err  error
	)
	if c.ParserFile == "" {
		data, err = readLogReport(logPath)
	} else {
		var rel string
		if rel, err = reportPath(job.TaskRelDir, c.ParserFile); err == nil {
			data, err = ex.readReport(ctx, job, rel)
		}
	}
	if err != nil {
		return nil, err
	}
	return testreport.Parse(c.Parser, bytes.NewReader(data))
}

// reportPath resolves `parser_file:` - relative to the task directory, like
// every other path in task.yaml - against the workspace root. Metadata
// validation already refuses one that escapes; this refuses it again, because
// the value reaches a file open and a container path.
func reportPath(taskRelDir, file string) (string, error) {
	rel := path.Join(taskRelDir, filepath.ToSlash(file))
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("parser_file %q escapes the workspace", file)
	}
	return rel, nil
}

// readLogReport reads a check's log file, one byte past the parser's own bound
// so an oversized report is refused rather than silently truncated.
func readLogReport(logPath string) ([]byte, error) {
	if logPath == "" {
		return nil, errors.New("check wrote no log to parse")
	}
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, testreport.MaxInput+1))
}

// boundedBuffer keeps the first max bytes written to it and silently discards
// the rest. Discarding rather than failing keeps the writer - a `docker exec`
// pipe - draining to the end instead of deadlocking on a reader that stopped;
// one byte over the bound is already enough for the parser to refuse the
// report.
type boundedBuffer struct {
	buf []byte
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
