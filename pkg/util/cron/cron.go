// Package cron parses the standard five-field cron expressions CronJobs use
// and finds the times they fire, like the robfig/cron parser Kubernetes
// relies on: minute hour day-of-month month day-of-week, each a list of
// values, ranges and steps ("*/15", "1-5", "MON-FRI", "JAN,JUL"), or a
// descriptor such as @hourly.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression: a bit set of allowed values per field.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	// domStar and dowStar record "*" (or "?"), which changes how the two
	// day fields combine.
	domStar, dowStar bool
}

type bounds struct {
	min, max uint
	names    map[string]uint
}

var (
	minutes = bounds{0, 59, nil}
	hours   = bounds{0, 23, nil}
	doms    = bounds{1, 31, nil}
	months  = bounds{1, 12, map[string]uint{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}}
	dows = bounds{0, 6, map[string]uint{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}}
)

var descriptors = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *", "@weekly": "0 0 * * 0",
	"@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

// Parse parses a cron expression; errors are worded like robfig/cron's.
func Parse(spec string) (*Schedule, error) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "@") {
		expanded, ok := descriptors[strings.ToLower(spec)]
		if !ok {
			return nil, fmt.Errorf("unrecognized descriptor: %s", spec)
		}
		spec = expanded
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("expected exactly 5 fields, found %d: %v", len(fields), fields)
	}
	s := &Schedule{}
	var err error
	for i, f := range []struct {
		dst *uint64
		b   bounds
	}{{&s.minute, minutes}, {&s.hour, hours}, {&s.dom, doms}, {&s.month, months}, {&s.dow, dows}} {
		if *f.dst, err = parseField(fields[i], f.b); err != nil {
			return nil, err
		}
	}
	s.domStar = fields[2] == "*" || fields[2] == "?"
	s.dowStar = fields[4] == "*" || fields[4] == "?"
	return s, nil
}

func parseField(field string, b bounds) (uint64, error) {
	var bits uint64
	for _, expr := range strings.Split(field, ",") {
		r, err := parseRange(expr, b)
		if err != nil {
			return 0, err
		}
		bits |= r
	}
	return bits, nil
}

// parseRange parses one of "*", "?", "5", "1-5", "*/2", "1-30/5", "10/5"
// or a name.
func parseRange(expr string, b bounds) (uint64, error) {
	rangeAndStep := strings.Split(expr, "/")
	lowAndHigh := strings.Split(rangeAndStep[0], "-")
	var start, end uint
	singleDigit := len(lowAndHigh) == 1
	if lowAndHigh[0] == "*" || lowAndHigh[0] == "?" {
		start, end = b.min, b.max
	} else {
		var err error
		if start, err = parseValue(lowAndHigh[0], b); err != nil {
			return 0, err
		}
		switch len(lowAndHigh) {
		case 1:
			end = start
		case 2:
			if end, err = parseValue(lowAndHigh[1], b); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("too many hyphens: %s", expr)
		}
	}
	step := uint(1)
	switch len(rangeAndStep) {
	case 1:
	case 2:
		n, err := strconv.Atoi(rangeAndStep[1])
		if err != nil {
			return 0, fmt.Errorf("failed to parse int from %s: %v", rangeAndStep[1], err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("step of range should be a positive number: %s", expr)
		}
		step = uint(n)
		if singleDigit && lowAndHigh[0] != "*" && lowAndHigh[0] != "?" {
			end = b.max // "N/step" runs from N to the maximum
		}
	default:
		return 0, fmt.Errorf("too many slashes: %s", expr)
	}
	switch {
	case start < b.min:
		return 0, fmt.Errorf("beginning of range (%d) below minimum (%d): %s", start, b.min, expr)
	case end > b.max:
		return 0, fmt.Errorf("end of range (%d) above maximum (%d): %s", end, b.max, expr)
	case start > end:
		return 0, fmt.Errorf("beginning of range (%d) beyond end of range (%d): %s", start, end, expr)
	}
	var bits uint64
	for v := start; v <= end; v += step {
		bits |= 1 << v
	}
	return bits, nil
}

func parseValue(s string, b bounds) (uint, error) {
	if v, ok := b.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("failed to parse int from %s: %v", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative number (%d) not allowed: %s", n, s)
	}
	return uint(n), nil
}

// dayMatches combines the two day fields like cron: if either is "*",
// both must match; otherwise either may.
func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}

// Next returns the first time after t the schedule fires, in t's location,
// or the zero time if it never does within five years.
func (s *Schedule) Next(t time.Time) time.Time {
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.Year() + 5
wrap:
	if t.Year() > limit {
		return time.Time{}
	}
	for s.month&(1<<uint(t.Month())) == 0 {
		t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		if t.Month() == time.January {
			goto wrap
		}
	}
	for !s.dayMatches(t) {
		t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		if t.Day() == 1 {
			goto wrap
		}
	}
	for s.hour&(1<<uint(t.Hour())) == 0 {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		if t.Hour() == 0 {
			goto wrap
		}
	}
	for s.minute&(1<<uint(t.Minute())) == 0 {
		t = t.Add(time.Minute)
		if t.Minute() == 0 {
			goto wrap
		}
	}
	return t
}
