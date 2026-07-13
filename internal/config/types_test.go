package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestTimestampRequiresExplicitOffset(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"rfc3339 with offset", "2026-09-24T23:59:59+03:00", false},
		{"rfc3339 utc z", "2026-09-24T23:59:59Z", false},
		{"bare date", "2026-09-24", true},
		{"space separated no offset", "2026-09-24 23:59:59", true},
		{"datetime no offset", "2026-09-24T23:59:59", true},
		{"garbage", "not-a-time", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var doc struct {
				Val Timestamp `yaml:"val"`
			}
			err := yaml.Unmarshal([]byte("val: "+tc.in), &doc)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got none", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
		})
	}
}

func TestDurationDecode(t *testing.T) {
	var doc struct {
		Val Duration `yaml:"val"`
	}
	if err := yaml.Unmarshal([]byte("val: 5m"), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Val.Std(); got != 5*time.Minute {
		t.Fatalf("got %v, want 5m", got)
	}
	if err := yaml.Unmarshal([]byte("val: notaduration"), &doc); err == nil {
		t.Fatal("expected error for bad duration")
	}
}

func TestByteSizeDecode(t *testing.T) {
	cases := map[string]int64{
		"512m": 512 << 20,
		"1g":   1 << 30,
		"2048": 2048,
		"1k":   1 << 10,
	}
	for in, want := range cases {
		var doc struct {
			Val ByteSize `yaml:"val"`
		}
		if err := yaml.Unmarshal([]byte("val: "+in), &doc); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := doc.Val.Bytes(); got != want {
			t.Fatalf("%s: got %d, want %d", in, got, want)
		}
	}
	var doc struct {
		Val ByteSize `yaml:"val"`
	}
	if err := yaml.Unmarshal([]byte("val: 5x"), &doc); err == nil {
		t.Fatal("expected error for bad byte size")
	}
}
