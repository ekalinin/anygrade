package web

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// tokenSpan matches the only markup the highlighter is allowed to write.
// Removing it from the output leaves exactly what came out of the file, which
// must not contain a markup character at all.
var tokenSpan = regexp.MustCompile(`<span class="tok-[a-z]+">|</span>`)

// hostile is a line that is valid source in several languages and markup in
// none of them - it only becomes markup if something forgets to escape it.
const hostile = `x = "<script>alert('pwn')</script>" # <img src=x onerror=alert(1)> & "`

// assertNoMarkup fails when anything outside the token spans could be parsed as
// markup by a browser.
func assertNoMarkup(t *testing.T, what, got string) {
	t.Helper()
	if rest := tokenSpan.ReplaceAllString(got, ""); strings.ContainsAny(rest, `<>"'`) {
		t.Errorf("%s: the file's own bytes reached the page unescaped: %q", what, rest)
	}
}

// TestHighlightNeverEmitsMarkupFromTheFile is the test this feature stands on.
// The highlighted file is a blob out of a student's commit, and it is rendered
// into a teacher's session, so a single unescaped byte is stored XSS against
// the account that can rewrite everybody's grades. The property checked is not
// "this input is escaped" but "the only markup in the output is markup the
// server wrote": every language, including the one the highlighter does not
// know and renders plain.
func TestHighlightNeverEmitsMarkupFromTheFile(t *testing.T) {
	for _, name := range []string{
		"solution.go", "solution.py", "run.sh", "task.yaml",
		"main.c", "app.js", "lib.rs", "notes.unknown",
	} {
		got := string(highlightFile(hostile, langFor(name)))
		assertNoMarkup(t, name, got)
		if !strings.Contains(got, "&lt;script&gt;") {
			t.Errorf("%s: the script tag is not in the output at all: %q", name, got)
		}
	}
}

// TestDiffNeverEmitsMarkupFromTheFile: the diff view highlights both versions
// line by line, so it is a second path onto the page and needs the same proof.
func TestDiffNeverEmitsMarkupFromTheFile(t *testing.T) {
	ops, ok := lineDiff([]string{"x = 1"}, []string{hostile})
	if !ok {
		t.Fatal("lineDiff declined a two-line diff")
	}
	rows := renderDiff(ops, langFor("solution.py"))
	if len(rows) == 0 {
		t.Fatal("renderDiff returned no rows for a changed file")
	}
	for _, row := range rows {
		assertNoMarkup(t, "diff row", string(row.Text))
	}
}

func TestLineDiff(t *testing.T) {
	cases := []struct {
		name     string
		old, new []string
		want     []diffOp
	}{
		{
			name: "identical",
			old:  []string{"a", "b"}, new: []string{"a", "b"},
			want: []diffOp{{"ctx", "a"}, {"ctx", "b"}},
		},
		{
			name: "pure insertion",
			old:  []string{"a", "c"}, new: []string{"a", "b", "c"},
			want: []diffOp{{"ctx", "a"}, {"add", "b"}, {"ctx", "c"}},
		},
		{
			name: "pure deletion",
			old:  []string{"a", "b", "c"}, new: []string{"a", "c"},
			want: []diffOp{{"ctx", "a"}, {"del", "b"}, {"ctx", "c"}},
		},
		{
			name: "interleaved change",
			old:  []string{"a", "b", "c", "d"}, new: []string{"a", "x", "c", "y"},
			want: []diffOp{
				{"ctx", "a"}, {"del", "b"}, {"add", "x"},
				{"ctx", "c"}, {"del", "d"}, {"add", "y"},
			},
		},
		{
			name: "new file",
			old:  nil, new: []string{"a", "b"},
			want: []diffOp{{"add", "a"}, {"add", "b"}},
		},
		{
			name: "emptied file",
			old:  []string{"a"}, new: nil,
			want: []diffOp{{"del", "a"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := lineDiff(tc.old, tc.new)
			if !ok {
				t.Fatal("lineDiff declined the input")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("lineDiff = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLineDiffDeclinesAnUnboundedTable: the table is quadratic, so past the cap
// the page offers no diff instead of allocating gigabytes on one click.
func TestLineDiffDeclinesAnUnboundedTable(t *testing.T) {
	n := 2000
	from := make([]string, n)
	to := make([]string, n)
	for i := range n {
		from[i] = "old " + itoa(int64(i))
		to[i] = "new " + itoa(int64(i))
	}
	if _, ok := lineDiff(from, to); ok {
		t.Errorf("lineDiff accepted a %dx%d table (%d cells > %d)", n, n, n*n, diffMaxCells)
	}
}

// TestRenderDiffCollapsesUnchangedRuns: a teacher opens the page to see what
// changed, so the untouched bulk of the file becomes one gap row.
func TestRenderDiffCollapsesUnchangedRuns(t *testing.T) {
	from := make([]string, 20)
	for i := range from {
		from[i] = "line " + itoa(int64(i))
	}
	to := slices.Clone(from)
	to[10] = "changed"

	ops, ok := lineDiff(from, to)
	if !ok {
		t.Fatal("lineDiff declined the input")
	}
	rows := renderDiff(ops, nil)

	var kinds []string
	skipped := 0
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
		skipped += r.Skipped
	}
	want := []string{
		"gap", "ctx", "ctx", "ctx", "del", "add", "ctx", "ctx", "ctx", "gap",
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("row kinds = %v, want %v", kinds, want)
	}
	// 19 unchanged lines, 6 of them kept around the change.
	if skipped != 13 {
		t.Errorf("collapsed %d lines, want 13", skipped)
	}
}

// TestRenderDiffOfAnUnchangedFileIsEmpty: nil rows are how the page tells "the
// file still matches the template" apart from "here is the delta".
func TestRenderDiffOfAnUnchangedFileIsEmpty(t *testing.T) {
	ops, ok := lineDiff([]string{"a", "b"}, []string{"a", "b"})
	if !ok {
		t.Fatal("lineDiff declined the input")
	}
	if rows := renderDiff(ops, nil); rows != nil {
		t.Errorf("renderDiff of an identical file = %v, want nil", rows)
	}
}

// TestHighlightMarksTokens: the point of the pass, on the one language it is
// most likely to see.
func TestHighlightMarksTokens(t *testing.T) {
	got := string(highlightFile("func main() { s := \"hi\" } // done", langFor("main.go")))
	for _, want := range []string{
		`<span class="tok-key">func</span>`,
		`<span class="tok-string">&#34;hi&#34;</span>`,
		`<span class="tok-comment">// done</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("highlighted Go source lacks %s:\n%s", want, got)
		}
	}
}

// TestHighlightOfAnUnknownLanguageIsPlain: an extension the table does not know
// renders exactly as the page did before this existed.
func TestHighlightOfAnUnknownLanguageIsPlain(t *testing.T) {
	const src = "func main() { }\nsecond line"
	if got := string(highlightFile(src, langFor("notes.unknown"))); got != src {
		t.Errorf("highlightFile = %q, want the source verbatim", got)
	}
}

// TestSplitLinesDropsThePhantomLastLine: a file ending in a newline has no
// empty last line, and reporting one would make every such file differ from a
// template that does not end in one.
func TestSplitLinesDropsThePhantomLastLine(t *testing.T) {
	cases := map[string][]string{
		"":           nil,
		"a\n":        {"a"},
		"a\nb":       {"a", "b"},
		"a\r\nb\r\n": {"a", "b"},
		"a\n\n":      {"a", ""},
	}
	for in, want := range cases {
		if got := splitLines(in); !slices.Equal(got, want) {
			t.Errorf("splitLines(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSolutionFile(t *testing.T) {
	cases := []struct {
		relDir, relPath string
		want            bool
	}{
		{"tasks/one", "tasks/one/main.go", true},
		{"tasks/one", "tasks/one/README.md", false},
		{"tasks/one", "tasks/two/main.go", false},
		{"", "main.go", true}, // a task at the repo root
	}
	for _, tc := range cases {
		if got := isSolutionFile(tc.relDir, []string{"main.go"}, tc.relPath); got != tc.want {
			t.Errorf("isSolutionFile(%q, %q) = %v, want %v", tc.relDir, tc.relPath, got, tc.want)
		}
	}
}
