package sqlitex

import (
	"database/sql"
	"testing"
	"time"
)

// TestFormatTimeSortsChronologically guards the fixed-width layout invariant:
// stored timestamp text must sort lexicographically in chronological order so
// timers, ORDER BY, and lease-expiry sweeps can compare it directly.
func TestFormatTimeSortsChronologically(t *testing.T) {
	earlier := time.Date(2026, 1, 2, 3, 4, 5, 500_000_000, time.UTC) // .5
	later := time.Date(2026, 1, 2, 3, 4, 5, 510_000_000, time.UTC)   // .51
	if FormatTime(earlier) >= FormatTime(later) {
		t.Fatalf("timestamp text not chronologically sortable: %q >= %q", FormatTime(earlier), FormatTime(later))
	}
}

func TestParseTimeRoundTripsUTC(t *testing.T) {
	original := time.Date(2026, 6, 12, 21, 0, 0, 123_456_789, time.FixedZone("PST", -8*3600))
	parsed, err := ParseTime(FormatTime(original))
	if err != nil {
		t.Fatalf("ParseTime: %v", err)
	}
	if !parsed.Equal(original) {
		t.Fatalf("round-trip mismatch: got %v want %v", parsed, original)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("ParseTime did not normalize to UTC: %v", parsed.Location())
	}
}

func TestNullableString(t *testing.T) {
	blank := "   "
	value := "  hello  "
	for _, tc := range []struct {
		name string
		in   *string
		want any
	}{
		{"nil", nil, nil},
		{"blank", &blank, nil},
		{"trimmed", &value, "hello"},
	} {
		if got := NullableString(tc.in); got != tc.want {
			t.Errorf("%s: NullableString = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNullableNonEmptyString(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want any
	}{
		{"empty", "", nil},
		{"blank", "   ", nil},
		{"trimmed", "  hello  ", "hello"},
	} {
		if got := NullableNonEmptyString(tc.in); got != tc.want {
			t.Errorf("%s: NullableNonEmptyString(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestNullableStringPointer(t *testing.T) {
	if got := NullableStringPointer(sql.NullString{}); got != nil {
		t.Errorf("invalid NullString should map to nil, got %v", *got)
	}
	if got := NullableStringPointer(sql.NullString{String: "x", Valid: true}); got == nil || *got != "x" {
		t.Errorf("valid NullString should map to pointer to value, got %v", got)
	}
}
