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
		rec := []string{row.User.Login, row.User.DisplayName}
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

func taskIDs(tasks []TaskCol) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return ids
}

// FmtScore renders a score the way the UI does: one decimal, ".0" trimmed.
func FmtScore(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0")
}
