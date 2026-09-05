package web

import (
	"html/template"
	"path"
	"slices"
	"strings"
)

// This file renders one submitted file for the teacher: the line diff against
// the authoritative course version, and the syntax highlighting both views
// share. Both are hand-rolled and dependency-free on purpose - the project
// ships no frontend toolchain and no CDN (the landing page highlights the same
// way), and a diff library in go.mod would be a heavy price for what a
// line-level LCS does in fifty lines.
//
// Everything here reads a blob out of a student's commit, so the input is
// untrusted: the text is escaped first and wrapped in spans afterwards, never
// the other way round. See highlightLine for why the template.HTML values
// below cannot carry markup that came from the file.

// diffOp is one line of the line-level diff; kind is "ctx", "del" or "add".
type diffOp struct {
	Kind string
	Text string
}

// diffMaxCells caps the LCS table. The algorithm is O(n*m) in time and memory
// and the code view inlines files up to codeMaxInline, so an unbounded table is
// a whole-server memory spike behind one teacher's click. Past the cap the page
// simply offers no diff and stays on the plain view. The common prefix and
// suffix are trimmed before the check, so what the cap really bounds is the
// changed region, not the file.
const diffMaxCells = 1 << 20

// lineDiff is a line-level LCS diff of from -> to. ok is false when the changed
// region is past diffMaxCells.
func lineDiff(from, to []string) (ops []diffOp, ok bool) {
	// The common prefix and suffix are context by definition and cost a linear
	// scan; skipping them keeps the quadratic part to the lines that differ,
	// which for a solution against its template is nearly always a handful.
	head := 0
	for head < len(from) && head < len(to) && from[head] == to[head] {
		head++
	}
	tail := 0
	for tail < len(from)-head && tail < len(to)-head &&
		from[len(from)-1-tail] == to[len(to)-1-tail] {
		tail++
	}
	a, b := from[head:len(from)-tail], to[head:len(to)-tail]
	if len(a)*len(b) > diffMaxCells {
		return nil, false
	}

	// lcs[i*(m+1)+j] is the LCS length of a[i:] and b[j:]; computing it from
	// the suffixes lets the emit walk run forwards, in reading order.
	n, m := len(a), len(b)
	lcs := make([]int32, (n+1)*(m+1))
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j+1] + 1
			case lcs[(i+1)*(m+1)+j] >= lcs[i*(m+1)+j+1]:
				lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j]
			default:
				lcs[i*(m+1)+j] = lcs[i*(m+1)+j+1]
			}
		}
	}

	ops = make([]diffOp, 0, head+tail+n+m)
	for _, line := range from[:head] {
		ops = append(ops, diffOp{"ctx", line})
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{"ctx", a[i]})
			i, j = i+1, j+1
		// A tie deletes first, so a replaced line reads "-old" then "+new"
		// rather than the other way round.
		case lcs[(i+1)*(m+1)+j] >= lcs[i*(m+1)+j+1]:
			ops = append(ops, diffOp{"del", a[i]})
			i++
		default:
			ops = append(ops, diffOp{"add", b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{"del", a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{"add", b[j]})
	}
	for _, line := range from[len(from)-tail:] {
		ops = append(ops, diffOp{"ctx", line})
	}
	return ops, true
}

// diffRow is one rendered row of the diff view.
type diffRow struct {
	Kind string // "ctx", "del", "add", "gap"
	Mark string // gutter character; empty on a gap row
	// Text is highlighted HTML the server wrote (see highlightLine).
	Text template.HTML
	// Skipped is the number of unchanged lines a "gap" row stands for.
	Skipped int
}

// diffContext is how many unchanged lines stay around every change. The page
// exists to answer "what did the student change", so the untouched bulk of the
// file collapses into a gap row; the whole text is one click away.
const diffContext = 3

// renderDiff highlights the diff and collapses long runs of context. It returns
// nil when nothing changed, which is how the page tells "the file still matches
// the template" apart from "here is the delta".
func renderDiff(ops []diffOp, l *lang) []diffRow {
	keep := make([]bool, len(ops))
	changed := false
	for k, op := range ops {
		if op.Kind == "ctx" {
			continue
		}
		changed = true
		for x := max(0, k-diffContext); x <= min(len(ops)-1, k+diffContext); x++ {
			keep[x] = true
		}
	}
	if !changed {
		return nil
	}

	rows := make([]diffRow, 0, len(ops))
	for k := 0; k < len(ops); {
		if keep[k] {
			rows = append(rows, renderDiffRow(ops[k], l))
			k++
			continue
		}
		end := k
		for end < len(ops) && !keep[end] {
			end++
		}
		// A single hidden line saves no space and would read as "1 unchanged
		// lines"; print it instead.
		if end-k < 2 {
			for ; k < end; k++ {
				rows = append(rows, renderDiffRow(ops[k], l))
			}
			continue
		}
		rows = append(rows, diffRow{Kind: "gap", Skipped: end - k})
		k = end
	}
	return rows
}

func renderDiffRow(op diffOp, l *lang) diffRow {
	mark := " "
	switch op.Kind {
	case "add":
		mark = "+"
	case "del":
		mark = "-"
	}
	return diffRow{Kind: op.Kind, Mark: mark, Text: highlightLine(op.Text, l)}
}

// splitLines splits a file into lines without the phantom empty line a trailing
// newline would produce, and without the carriage returns a CRLF file carries:
// neither is visible in a <pre>, but both would make every line of such a file
// differ from a template written the other way.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

// isSolutionFile reports whether relPath is one of the task's solution_files.
// Those are the only files with an authoritative counterpart worth diffing:
// everything else in the workspace is restored from the course repo before the
// checks run, so its diff is empty by construction, and a file the student
// added has no counterpart at all.
func isSolutionFile(relDir string, solutionFiles []string, relPath string) bool {
	return slices.ContainsFunc(solutionFiles, func(sf string) bool {
		return path.Join(relDir, sf) == relPath
	})
}

// lang is what the highlighter knows about one language. Keyword lists are
// short and shared between relatives on purpose: this is a reading aid, and
// colouring a sibling dialect's keyword costs a reader nothing, while a
// per-dialect table would be a maintenance burden nobody grades on.
type lang struct {
	line      string // line comment marker ("" = none)
	blockOpen string // block comment delimiters ("" = none)
	blockEnd  string
	quotes    string // characters that open a string literal
	keywords  map[string]bool
}

func words(list string) map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(list) {
		m[w] = true
	}
	return m
}

var (
	goLang = &lang{line: "//", blockOpen: "/*", blockEnd: "*/", quotes: "\"'`", keywords: words(`
		break case chan const continue default defer else fallthrough for func go goto if import
		interface map package range return select struct switch type var nil true false`)}
	pyLang = &lang{line: "#", quotes: "\"'", keywords: words(`
		and as assert async await break class continue def del elif else except finally for from
		global if import in is lambda nonlocal not or pass raise return try while with yield
		None True False`)}
	jsLang = &lang{line: "//", blockOpen: "/*", blockEnd: "*/", quotes: "\"'`", keywords: words(`
		async await break case catch class const continue default delete do else export extends
		finally for from function if import in instanceof let new of return static super switch
		this throw try typeof var void while yield null true false`)}
	cLang = &lang{line: "//", blockOpen: "/*", blockEnd: "*/", quotes: "\"'", keywords: words(`
		auto bool break case catch char class const continue default delete do double else enum
		extends extern final float for goto if implements import inline int long namespace new
		package private protected public return short signed sizeof static struct switch template
		this throw try typedef typename union unsigned using virtual void volatile while
		null true false`)}
	rustLang = &lang{line: "//", blockOpen: "/*", blockEnd: "*/", quotes: "\"'", keywords: words(`
		as async await break const continue crate dyn else enum extern fn for if impl in let loop
		match mod move mut pub ref return self static struct super trait type unsafe use where
		while true false`)}
	shLang = &lang{line: "#", quotes: "\"'", keywords: words(`
		case do done elif else esac fi for function if in local readonly return then until while`)}
	yamlLang = &lang{line: "#", quotes: "\"'", keywords: words(`true false null yes no`)}
	jsonLang = &lang{quotes: "\"", keywords: words(`true false null`)}
)

// langs maps a file extension to its highlighter. An extension that is not here
// renders plain, exactly as this page did before highlighting existed.
var langs = map[string]*lang{
	".go":   goLang,
	".py":   pyLang,
	".js":   jsLang,
	".mjs":  jsLang,
	".ts":   jsLang,
	".c":    cLang,
	".h":    cLang,
	".cc":   cLang,
	".cpp":  cLang,
	".hpp":  cLang,
	".java": cLang,
	".cs":   cLang,
	".rs":   rustLang,
	".sh":   shLang,
	".bash": shLang,
	".rb":   pyLang, // close enough: # comments, the same quoting
	".yaml": yamlLang,
	".yml":  yamlLang,
	".json": jsonLang,
}

func langFor(relPath string) *lang {
	return langs[strings.ToLower(path.Ext(relPath))]
}

// highlightFile renders a whole file; see highlightLine for the safety rule.
func highlightFile(src string, l *lang) template.HTML {
	lines := splitLines(src)
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = string(highlightLine(line, l))
	}
	return template.HTML(strings.Join(out, "\n"))
}

// highlightLine renders one source line as HTML.
//
// Safety: the line is a blob out of a student's commit, rendered into a
// teacher's session, so not one byte of it may reach the page as markup. Every
// byte goes through template.HTMLEscapeString and the <span> wrappers are
// constants written right here, so the returned template.HTML is markup the
// server wrote and never markup the file carried. That, and only that, is what
// makes it safe to hand html/template a pre-escaped value.
//
// The pass is per line and keeps no state between lines: the diff view
// interleaves lines from two versions of the file, where a string or a block
// comment opened in one says nothing about the other. An unterminated one
// therefore colours to the end of its own line, which is the honest answer for
// a line shown out of context.
func highlightLine(src string, l *lang) template.HTML {
	if l == nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	var b strings.Builder
	plain := 0 // start of the run that gets no span
	flush := func(to int) {
		b.WriteString(template.HTMLEscapeString(src[plain:to]))
	}
	emit := func(class, text string) {
		b.WriteString(`<span class="tok-`)
		b.WriteString(class)
		b.WriteString(`">`)
		b.WriteString(template.HTMLEscapeString(text))
		b.WriteString(`</span>`)
	}
	// afterIdent guards the number and keyword cases: `x1` is one identifier,
	// not an identifier followed by a number.
	afterIdent := func(i int) bool { return i > 0 && identByte(src[i-1]) }

	for i := 0; i < len(src); {
		switch {
		case l.line != "" && strings.HasPrefix(src[i:], l.line):
			flush(i)
			emit("comment", src[i:])
			return template.HTML(b.String())

		case l.blockOpen != "" && strings.HasPrefix(src[i:], l.blockOpen):
			end := len(src)
			if k := strings.Index(src[i+len(l.blockOpen):], l.blockEnd); k >= 0 {
				end = i + len(l.blockOpen) + k + len(l.blockEnd)
			}
			flush(i)
			emit("comment", src[i:end])
			i, plain = end, end

		case strings.IndexByte(l.quotes, src[i]) >= 0:
			end := stringEnd(src, i)
			flush(i)
			emit("string", src[i:end])
			i, plain = end, end

		case src[i] >= '0' && src[i] <= '9' && !afterIdent(i):
			end := i
			for end < len(src) && (identByte(src[end]) || src[end] == '.') {
				end++
			}
			flush(i)
			emit("num", src[i:end])
			i, plain = end, end

		case identByte(src[i]) && !afterIdent(i):
			end := i
			for end < len(src) && identByte(src[end]) {
				end++
			}
			if l.keywords[src[i:end]] {
				flush(i)
				emit("key", src[i:end])
				plain = end
			}
			// A plain identifier stays in the unstyled run: skipping past it
			// also keeps its digits from being read as a number.
			i = end

		default:
			i++
		}
	}
	flush(len(src))
	return template.HTML(b.String())
}

// identByte reports whether c can appear inside an identifier. Bytes >= 0x80
// count, so a non-ASCII identifier is consumed whole and the slicing above
// never cuts a rune in half - every other cut is at an ASCII delimiter.
func identByte(c byte) bool {
	return c == '_' || c >= 0x80 ||
		c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// stringEnd returns the index just past the string literal opened at i, or the
// end of the line when it never closes. A backslash escapes the next byte
// except inside a backtick literal, where Go takes it literally.
func stringEnd(src string, i int) int {
	quote := src[i]
	for k := i + 1; k < len(src); k++ {
		switch {
		case src[k] == '\\' && quote != '`':
			k++
		case src[k] == quote:
			return k + 1
		}
	}
	return len(src)
}
