// Package cron parses the five-field cron expressions of forecrew.yaml
// schedules and finds the times they fire. It is deliberately small: UTC
// only, numbers, lists, ranges and steps, and the shortcuts @hourly, @daily,
// @weekly, @monthly and @yearly. A library would bring time zones and
// seconds that nothing here uses.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid is wrapped by every error of Parse.
var ErrInvalid = errors.New("invalid cron expression")

// Schedule is a parsed cron expression: the minutes of an hour, the hours
// of a day, the days of a month and week, and the months of a year at which
// it fires.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	// domStar and dowStar say a day field was *: as in cron, when neither is,
	// a day matches either field.
	domStar, dowStar bool
}

var shortcuts = map[string]string{
	"@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *",
	"@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *",
}

// Parse reads "minute hour day-of-month month day-of-week". Each field is *,
// a number, a range a-b, a list of those, and may end in /step. Months are
// 1-12 and days of the week 0-6 (Sunday is 0; 7 is accepted for Sunday).
func Parse(spec string) (Schedule, error) {
	if s, ok := shortcuts[strings.TrimSpace(spec)]; ok {
		spec = s
	}
	f := strings.Fields(spec)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("%w %q: want 5 fields (minute hour day-of-month month day-of-week)", ErrInvalid, spec)
	}
	var s Schedule
	var err error
	for i, f := range []struct {
		text     string
		min, max int
		out      *uint64
	}{{f[0], 0, 59, &s.minute}, {f[1], 0, 23, &s.hour}, {f[2], 1, 31, &s.dom}, {f[3], 1, 12, &s.month}, {f[4], 0, 7, &s.dow}} {
		if *f.out, err = field(f.text, f.min, f.max); err != nil {
			return Schedule{}, fmt.Errorf("%w %q: field %d: %w", ErrInvalid, spec, i+1, err)
		}
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow = s.dow&^(1<<7) | 1
	}
	s.domStar, s.dowStar = f[2][0] == '*', f[4][0] == '*'
	return s, nil
}

// field returns the bit set of one field.
func field(text string, lo, hi int) (uint64, error) {
	var bits uint64
	for part := range strings.SplitSeq(text, ",") {
		from, to, step, err := parts(part, lo, hi)
		if err != nil {
			return 0, err
		}
		for v := from; v <= to; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

// parts reads one item of a list: *, n or a-b, with an optional /step.
func parts(part string, lo, hi int) (from, to, step int, err error) {
	rng, stepText, hasStep := strings.Cut(part, "/")
	step = 1
	if hasStep {
		if step, err = strconv.Atoi(stepText); err != nil || step < 1 {
			return 0, 0, 0, fmt.Errorf("step %q must be a positive number", stepText)
		}
	}
	from, to = lo, hi
	if rng != "*" {
		a, b, isRange := strings.Cut(rng, "-")
		if from, err = strconv.Atoi(a); err != nil {
			return 0, 0, 0, fmt.Errorf("%q is not a number", a)
		}
		to = from
		switch {
		case isRange:
			if to, err = strconv.Atoi(b); err != nil {
				return 0, 0, 0, fmt.Errorf("%q is not a number", b)
			}
		case hasStep:
			to = hi // 5/15 means 5-max/15
		}
	}
	if from < lo || to > hi || from > to {
		return 0, 0, 0, fmt.Errorf("%q is outside %d-%d", rng, lo, hi)
	}
	return from, to, step, nil
}

func has(set uint64, v int) bool { return set&(1<<v) != 0 }

func (s Schedule) dayMatches(t time.Time) bool {
	dom, dow := has(s.dom, t.Day()), has(s.dow, int(t.Weekday()))
	if !s.domStar && !s.dowStar {
		return dom || dow
	}
	return dom && dow
}

// searchYears bounds the search for a time: an expression that never fires
// (31 February) has none within it.
const searchYears = 8

// Next returns the first time after t, to the minute and in UTC, at which s
// fires; false when it does not within eight years.
func (s Schedule) Next(t time.Time) (time.Time, bool) {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	for end := t.AddDate(searchYears, 0, 0); t.Before(end); {
		switch {
		case !has(s.month, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case !has(s.hour, t.Hour()):
			t = t.Truncate(time.Hour).Add(time.Hour)
		case !has(s.minute, t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}

// Prev returns the last time at or before t, to the minute and in UTC, at
// which s fires; false when it did not within eight years.
func (s Schedule) Prev(t time.Time) (time.Time, bool) {
	t = t.UTC().Truncate(time.Minute)
	for end := t.AddDate(-searchYears, 0, 0); t.After(end); {
		switch {
		case !has(s.month, int(t.Month())):
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Add(-time.Minute)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(-time.Minute)
		case !has(s.hour, t.Hour()):
			t = t.Truncate(time.Hour).Add(-time.Minute)
		case !has(s.minute, t.Minute()):
			t = t.Add(-time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}
