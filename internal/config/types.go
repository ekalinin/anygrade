// Package config loads, merges, and validates anygrade course metadata
// (course.yaml and task.yaml). It exposes two layers: a raw yaml-facing layer
// (pointer-heavy, strict-decoded) and a resolved layer (plain value types with
// defaults merged in). Only the resolved layer should be consumed by the rest
// of the application.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a yaml-decodable time.Duration accepting Go duration strings
// ("5m", "24h", "0s").
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML decodes a duration string via time.ParseDuration.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar string")
	}
	parsed, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// ByteSize is a yaml-decodable byte count accepting suffixes k/m/g (powers of
// 1024), e.g. "512m", "1g", "2048".
type ByteSize int64

// Bytes returns the value in bytes.
func (b ByteSize) Bytes() int64 { return int64(b) }

// UnmarshalYAML decodes a byte-size string.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("byte size must be a scalar string")
	}
	v, err := parseByteSize(n.Value)
	if err != nil {
		return err
	}
	*b = ByteSize(v)
	return nil
}

func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty byte size")
	}
	mult := int64(1)
	numPart := s
	switch last := s[len(s)-1]; last {
	case 'k', 'K':
		mult, numPart = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mult, numPart = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mult, numPart = 1<<30, s[:len(s)-1]
	case 'b', 'B':
		mult, numPart = 1, s[:len(s)-1]
	default:
		if last < '0' || last > '9' {
			return 0, fmt.Errorf("invalid byte size %q", s)
		}
	}
	val, err := strconv.ParseFloat(strings.TrimSpace(numPart), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
	}
	if val < 0 {
		return 0, fmt.Errorf("negative byte size %q", s)
	}
	return int64(val * float64(mult)), nil
}

// Timestamp is a yaml-decodable time enforcing RFC 3339 with an explicit
// offset. Bare dates and space-separated datetimes (no offset) are rejected,
// per SPEC §4.3.
type Timestamp time.Time

// Std returns the value as a time.Time.
func (t Timestamp) Std() time.Time { return time.Time(t) }

// UnmarshalYAML decodes a timestamp, requiring RFC3339 with an explicit offset.
func (t *Timestamp) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("timestamp must be a scalar string")
	}
	// Parse the raw scalar text directly (not via yaml's !!timestamp
	// resolution) so that offset-less values fail.
	parsed, err := time.Parse(time.RFC3339, n.Value)
	if err != nil {
		return fmt.Errorf("timestamp %q must be RFC3339 with an explicit offset: %w", n.Value, err)
	}
	*t = Timestamp(parsed)
	return nil
}
