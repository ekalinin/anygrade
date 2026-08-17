package gradebook

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// WriteCSV emits the score matrix: login, display_name, one column per task
// id (stable, unlike names), total. Cells show the display score (override
// wins), formatted like the UI.
func WriteCSV(w io.Writer, m Matrix) error {
	cw := csv.NewWriter(w)
	header := append([]string{"login", "display_name"}, taskIDs(m.Tasks)...)
	header = append(header, "total")
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, row := range m.Rows {
		rec := []string{csvSafe(row.User.Login), csvSafe(row.User.DisplayName)}
		for _, t := range m.Tasks {
			rec = append(rec, FmtScore(row.Cells[t.ID].Display))
		}
		rec = append(rec, FmtScore(row.Total))
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// csvSafe neutralizes spreadsheet formula injection. Excel, LibreOffice, and
// Sheets evaluate a cell that starts with =, +, - or @ as a formula, and a
// display name comes straight out of open registration - so an export the
// teacher opens would run whatever the student typed. The conventional escape
// is a leading apostrophe: spreadsheets strip it on import and show the literal
// text, and a plain CSV reader sees one extra character.
//
// Score cells are not run through this: they are formatted by FmtScore, never
// negative, and quoting them would break every consumer of the export.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

func taskIDs(tasks []TaskCol) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = csvSafe(t.ID)
	}
	return ids
}

// FmtScore renders a score the way the UI does: one decimal, ".0" trimmed.
func FmtScore(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0")
}
