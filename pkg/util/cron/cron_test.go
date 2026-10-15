package cron

import (
	"testing"
	"time"
)

func TestParseErrors(t *testing.T) {
	for spec, want := range map[string]string{
		"every minute":  "expected exactly 5 fields, found 2: [every minute]",
		"60 * * * *":    "end of range (60) above maximum (59): 60",
		"5-2 * * * *":   "beginning of range (5) beyond end of range (2): 5-2",
		"*/0 * * * *":   "step of range should be a positive number: */0",
		"x * * * *":     `failed to parse int from x: strconv.Atoi: parsing "x": invalid syntax`,
		"* * 0 * *":     "beginning of range (0) below minimum (1): 0",
		"@every 5m":     "unrecognized descriptor: @every 5m",
		"* * * * 1-2-3": "too many hyphens: 1-2-3",
	} {
		if _, err := Parse(spec); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", spec, err, want)
		}
	}
}

func TestNext(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct{ spec, from, want string }{
		{"*/1 * * * *", "2026-10-15 05:00", "2026-10-15 05:01"},
		{"*/15 * * * *", "2026-10-15 05:01", "2026-10-15 05:15"},
		{"0 * * * *", "2026-10-15 05:00", "2026-10-15 06:00"},
		{"@hourly", "2026-10-15 23:30", "2026-10-16 00:00"},
		{"30 9 * * MON-FRI", "2026-10-16 10:00", "2026-10-19 09:30"}, // Friday after 9:30: Monday
		{"0 0 1 JAN *", "2026-10-15 00:00", "2027-01-01 00:00"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"10/20 * * * *", "2026-10-15 05:11", "2026-10-15 05:30"},
		{"0 12 13 * 5", "2026-10-10 00:00", "2026-10-13 12:00"}, // the 13th, or any Friday: Tuesday the 13th
		{"0 0 31 2 *", "2026-01-01 00:00", "0001-01-01 00:00"},  // never
	} {
		s, err := Parse(tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.spec, err)
		}
		if got := s.Next(at(tc.from)); !got.Equal(at(tc.want)) {
			t.Errorf("%s after %s: %v, want %s", tc.spec, tc.from, got, tc.want)
		}
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := Parse("0 9 * * *")
	from := time.Date(2026, 10, 15, 10, 0, 0, 0, berlin)
	if got := s.Next(from); !got.Equal(time.Date(2026, 10, 16, 9, 0, 0, 0, berlin)) {
		t.Errorf("time zone: %v", got)
	}
}
