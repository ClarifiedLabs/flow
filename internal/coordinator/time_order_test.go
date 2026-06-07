package coordinator

import (
	"testing"
	"time"
)

// Regression: RFC3339Nano trims trailing fractional zeros, so ".5Z" sorted
// lexicographically AFTER ".51Z" and whole seconds after any fraction in the
// same second, breaking every ORDER BY / <= comparison on stored timestamp
// text. formatTime must render fixed-width fractions so string order equals
// chronological order, and parseTime must round-trip the new layout.
func TestFormatTimeLexicographicOrderIsChronological(t *testing.T) {
	base := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	ordered := []time.Time{
		base,
		base.Add(50 * time.Millisecond),  // .05
		base.Add(100 * time.Millisecond), // .1  (trimmed form ".1Z" used to sort after ".123Z")
		base.Add(123 * time.Millisecond), // .123
		base.Add(500 * time.Millisecond), // .5  (trimmed form ".5Z" used to sort after ".51Z")
		base.Add(510 * time.Millisecond), // .51
		base.Add(time.Second),            // whole second (trimmed form "…01Z" had no fraction)
	}

	for i := 1; i < len(ordered); i++ {
		prev := formatTime(ordered[i-1])
		curr := formatTime(ordered[i])
		if !(prev < curr) {
			t.Errorf("formatTime order broken: %q !< %q (times %v < %v)",
				prev, curr, ordered[i-1], ordered[i])
		}
	}

	for _, value := range ordered {
		parsed, err := parseTime(formatTime(value))
		if err != nil {
			t.Fatalf("parseTime(formatTime(%v)): %v", value, err)
		}
		if !parsed.Equal(value) {
			t.Errorf("round-trip mismatch: got %v, want %v", parsed, value)
		}
	}
}
