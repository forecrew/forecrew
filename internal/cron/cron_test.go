package cron_test

import (
	"errors"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/cron"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextAndPrev(t *testing.T) {
	tests := []struct {
		spec, from, next, prev string
	}{
		{"0 3 * * 1", "2026-11-05 10:00", "2026-11-09 03:00", "2026-11-02 03:00"},         // Monday 03:00
		{"0 3 * * 1", "2026-11-09 03:00", "2026-11-16 03:00", "2026-11-09 03:00"},         // at a slot: Prev is itself, Next the following one
		{"*/15 * * * *", "2026-11-05 10:07", "2026-11-05 10:15", "2026-11-05 10:00"},      // steps
		{"5-10/2,50 1 * * *", "2026-11-05 01:08", "2026-11-05 01:09", "2026-11-05 01:07"}, // ranges, lists, steps: 5,7,9,50
		{"5-10/2,50 1 * * *", "2026-11-05 01:09", "2026-11-05 01:50", "2026-11-05 01:09"}, //
		{"5-10/2,50 1 * * *", "2026-11-05 01:10", "2026-11-05 01:50", "2026-11-05 01:09"}, //
		{"0 0 1 * *", "2026-12-31 23:59", "2027-01-01 00:00", "2026-12-01 00:00"},         // year change
		{"30 4 29 2 *", "2026-11-05 00:00", "2028-02-29 04:30", "2024-02-29 04:30"},       // leap day
		{"0 12 13 * 5", "2026-11-01 00:00", "2026-11-06 12:00", "2026-10-30 12:00"},       // day of month or day of week, as cron does
		{"@daily", "2026-11-05 10:00", "2026-11-06 00:00", "2026-11-05 00:00"},            // shortcut
		{"@weekly", "2026-11-05 10:00", "2026-11-08 00:00", "2026-11-01 00:00"},           //
		{"0 0 * * 7", "2026-11-05 10:00", "2026-11-08 00:00", "2026-11-01 00:00"},         // 7 is Sunday
		{"0 9 * 3 *", "2026-11-05 10:00", "2027-03-01 09:00", "2026-03-31 09:00"},         // month
		{"0 0 * * 0-2", "2026-11-04 00:00", "2026-11-08 00:00", "2026-11-03 00:00"},       // day-of-week range
		{"7/20 * * * *", "2026-11-05 10:30", "2026-11-05 10:47", "2026-11-05 10:27"},      // 7,27,47
	}
	for _, tt := range tests {
		s, err := cron.Parse(tt.spec)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.spec, err)
		}
		if got, ok := s.Next(at(tt.from)); !ok || !got.Equal(at(tt.next)) {
			t.Errorf("%q: Next(%s) = %v, %v; want %s", tt.spec, tt.from, got, ok, tt.next)
		}
		if got, ok := s.Prev(at(tt.from)); !ok || !got.Equal(at(tt.prev)) {
			t.Errorf("%q: Prev(%s) = %v, %v; want %s", tt.spec, tt.from, got, ok, tt.prev)
		}
	}
}

// Seconds and the zone do not matter: times are minutes in UTC.
func TestTimesAreMinutesInUTC(t *testing.T) {
	s, err := cron.Parse("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("x", 2*3600)
	from := time.Date(2026, 11, 5, 4, 30, 59, 0, zone) // 02:30:59 UTC
	if got, _ := s.Next(from); !got.Equal(at("2026-11-05 03:00")) || got.Location() != time.UTC {
		t.Errorf("Next = %v", got)
	}
	if got, _ := s.Prev(from); !got.Equal(at("2026-11-05 03:00").AddDate(0, 0, -1)) {
		t.Errorf("Prev = %v", got)
	}
}

func TestNeverFires(t *testing.T) {
	s, err := cron.Parse("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Next(at("2026-11-05 10:00")); ok {
		t.Errorf("Next = %v", got)
	}
	if got, ok := s.Prev(at("2026-11-05 10:00")); ok {
		t.Errorf("Prev = %v", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, spec := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "a * * * *",
		"*/0 * * * *", "*/x * * * *", "5-3 * * * *", "1- * * * *", "@sometimes", "1,,2 * * * *", "-1 * * * *"} {
		if _, err := cron.Parse(spec); !errors.Is(err, cron.ErrInvalid) {
			t.Errorf("Parse(%q) = %v, want ErrInvalid", spec, err)
		}
	}
}
