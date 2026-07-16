package cli

import (
	"strings"
	"testing"
)

func TestParseRoster(t *testing.T) {
	t.Run("header detection and trim", func(t *testing.T) {
		csv := "login,display_name\n alice , Alice A \nbob,\n"
		got, err := parseRoster(strings.NewReader(csv))
		if err != nil {
			t.Fatal(err)
		}
		want := []rosterEntry{{Login: "alice", Name: "Alice A"}, {Login: "bob", Name: ""}}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("no header", func(t *testing.T) {
		got, err := parseRoster(strings.NewReader("alice,Alice A\nbob,Bob B\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Login != "alice" || got[1].Login != "bob" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("extra columns ignored", func(t *testing.T) {
		got, err := parseRoster(strings.NewReader("alice,Alice A,extra,more\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Login != "alice" || got[0].Name != "Alice A" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("invalid login reports 1-based row number", func(t *testing.T) {
		_, err := parseRoster(strings.NewReader("login\nalice\nBad Login\nbob\n"))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "row 3:") {
			t.Errorf("error = %q, want it to mention row 3", err)
		}
	})

	t.Run("empty login reports its row number", func(t *testing.T) {
		_, err := parseRoster(strings.NewReader("alice\n,Nobody\nbob\n"))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "row 2:") {
			t.Errorf("error = %q, want it to mention row 2", err)
		}
	})
}
