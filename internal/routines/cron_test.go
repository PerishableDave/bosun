package routines

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // deterministic zone data for the DST tests
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestParseCronErrors(t *testing.T) {
	for _, expr := range []string{
		"", "   ", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *",
		"* * * 13 *", "* * * * 8", "*/0 * * * *", "*/61 * * * *", "5-1 * * * *", "1,,2 * * * *",
		"a * * * *", "* * * FOO *", "@every 5m", "@reboot", "0 0 30 2 *", "0 0 31 4,6,9,11 *",
		"1-2-3 * * * *", "-1 * * * *", strings.Repeat("1,", 150) + "1 * * * *",
	} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) accepted", expr)
		}
	}
	_, err := ParseCron("61 * * * *")
	if err == nil || !strings.Contains(err.Error(), "minute") || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error does not name the field: %v", err)
	}
}

func TestCronNext(t *testing.T) {
	utc := time.UTC
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", s, utc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		expr, after string
		want        []string // successive activations
	}{
		{"* * * * *", "2026-10-12 10:00", []string{"2026-10-12 10:01", "2026-10-12 10:02"}},
		{"*/15 * * * *", "2026-10-12 10:07", []string{"2026-10-12 10:15", "2026-10-12 10:30", "2026-10-12 10:45", "2026-10-12 11:00"}},
		{"5/20 * * * *", "2026-10-12 10:00", []string{"2026-10-12 10:05", "2026-10-12 10:25", "2026-10-12 10:45", "2026-10-12 11:05"}},
		{"0 7 * * 1-5", "2026-10-09 07:00", []string{"2026-10-12 07:00", "2026-10-13 07:00"}}, // Fri -> Mon
		{"0 7 * * MON-FRI", "2026-10-10 12:00", []string{"2026-10-12 07:00"}},
		{"30 8-18/4 * * *", "2026-10-12 09:00", []string{"2026-10-12 12:30", "2026-10-12 16:30", "2026-10-13 08:30"}},
		{"0 0,12 1,15 * *", "2026-10-01 12:00", []string{"2026-10-15 00:00", "2026-10-15 12:00", "2026-11-01 00:00"}},
		{"0 0 * JAN,jul *", "2026-10-12 00:00", []string{"2027-01-01 00:00", "2027-01-02 00:00"}},
		{"0 0 29 2 *", "2026-01-01 00:00", []string{"2028-02-29 00:00", "2032-02-29 00:00"}},
		{"0 0 31 * *", "2026-04-01 00:00", []string{"2026-05-31 00:00", "2026-07-31 00:00"}},
		{"0 0 * * 7", "2026-10-12 00:00", []string{"2026-10-18 00:00", "2026-10-25 00:00"}}, // 7 = Sunday
		{"0 0 * * 0", "2026-10-12 00:00", []string{"2026-10-18 00:00"}},
		// Both day fields restricted: either matches (13th, or any Friday).
		{"0 0 13 * 5", "2026-10-12 00:00", []string{"2026-10-13 00:00", "2026-10-16 00:00", "2026-10-23 00:00"}},
		// Day-of-week restricted, day-of-month "*": both must match.
		{"0 0 * * 5", "2026-10-12 00:00", []string{"2026-10-16 00:00"}},
		{"@hourly", "2026-10-12 10:59", []string{"2026-10-12 11:00", "2026-10-12 12:00"}},
		{"@daily", "2026-10-12 00:00", []string{"2026-10-13 00:00"}},
		{"@midnight", "2026-10-12 23:59", []string{"2026-10-13 00:00"}},
		{"@weekly", "2026-10-12 00:00", []string{"2026-10-18 00:00", "2026-10-25 00:00"}},
		{"@monthly", "2026-10-12 00:00", []string{"2026-11-01 00:00"}},
		{"@yearly", "2026-10-12 00:00", []string{"2027-01-01 00:00"}},
		{"@ANNUALLY", "2026-10-12 00:00", []string{"2027-01-01 00:00"}},
		{"59 23 31 12 *", "2026-12-31 23:59", []string{"2027-12-31 23:59"}},
	}
	for _, c := range cases {
		s, err := ParseCron(c.expr)
		if err != nil {
			t.Errorf("ParseCron(%q): %v", c.expr, err)
			continue
		}
		cur := at(c.after)
		for _, w := range c.want {
			got := s.Next(cur, utc)
			if !got.Equal(at(w)) {
				t.Errorf("%q after %s = %s, want %s", c.expr, cur.Format("2006-01-02 15:04 Mon"), got.Format("2006-01-02 15:04 Mon"), w)
				break
			}
			cur = got
		}
	}
	// Seconds inside the "after" minute do not cause a re-fire.
	s, _ := ParseCron("* * * * *")
	if got := s.Next(at("2026-10-12 10:00").Add(30*time.Second), utc); !got.Equal(at("2026-10-12 10:01")) {
		t.Errorf("Next with seconds = %s", got)
	}
	if s.String() != "* * * * *" {
		t.Errorf("String = %q", s.String())
	}
}

func TestCronNextTimeZone(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	s, _ := ParseCron("0 7 * * *")
	got := s.Next(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), tokyo) // 09:00 in Tokyo
	want := time.Date(2026, 10, 13, 7, 0, 0, 0, tokyo)
	if !got.Equal(want) || got.Location() != tokyo {
		t.Errorf("Next = %s, want %s", got, want)
	}
}

func TestCronNextDST(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	in := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, ny) }
	check := func(expr string, after time.Time, wants ...time.Time) {
		t.Helper()
		s, err := ParseCron(expr)
		if err != nil {
			t.Fatal(err)
		}
		cur := after
		for i, w := range wants {
			got := s.Next(cur, ny)
			if !got.Equal(w) {
				t.Errorf("%q #%d after %s = %s, want %s", expr, i, cur, got, w)
				return
			}
			cur = got
		}
	}

	// Spring forward: 2026-03-08 02:00 EST -> 03:00 EDT.
	springGap := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC) // 03:00 EDT == 02:00 EST
	// A daily 02:30 job fires once at the end of the gap, then normally.
	s, _ := ParseCron("30 2 * * *")
	if got := s.Next(in(2026, 3, 7, 3, 0), ny); !got.Equal(springGap) {
		t.Errorf("02:30 on spring-forward day = %s, want %s", got, springGap)
	}
	if got := s.Next(springGap, ny); !got.Equal(in(2026, 3, 9, 2, 30)) {
		t.Errorf("day after spring-forward = %s", got)
	}
	// A daily 07:00 job is 23 real hours apart across the transition.
	check("0 7 * * *", in(2026, 3, 7, 7, 0), in(2026, 3, 8, 7, 0), in(2026, 3, 9, 7, 0))
	if d := in(2026, 3, 8, 7, 0).Sub(in(2026, 3, 7, 7, 0)); d != 23*time.Hour {
		t.Errorf("sanity: %v", d)
	}
	// Every-15-minutes collapses the missing hour into one activation at 03:00.
	check("*/15 * * * *", in(2026, 3, 8, 1, 45), springGap, in(2026, 3, 8, 3, 15), in(2026, 3, 8, 3, 30))
	// Hourly: 01:00 EST, then 03:00 EDT (02:00 does not exist), then 04:00.
	check("0 * * * *", in(2026, 3, 8, 0, 30), in(2026, 3, 8, 1, 0), springGap, in(2026, 3, 8, 4, 0))

	// Fall back: 2026-11-01 02:00 EDT -> 01:00 EST; 01:xx happens twice.
	firstOneThirty := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)  // 01:30 EDT
	secondOneThirty := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // 01:30 EST
	twoAM := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)            // 02:00 EST
	// A daily 01:30 job fires once, at the first occurrence.
	check("30 1 * * *", in(2026, 10, 31, 12, 0), firstOneThirty, in(2026, 11, 2, 1, 30))
	// Hourly at :30: the repeated hour does not fire twice.
	check("30 * * * *", time.Date(2026, 11, 1, 4, 45, 0, 0, time.UTC), firstOneThirty, time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC))
	// Starting inside the repeated hour (second pass) moves on to 02:00 EST,
	// never back in time.
	check("0,30 * * * *", secondOneThirty.Add(-10*time.Minute), twoAM)
	// Daily 07:00 is 25 real hours apart across fall-back.
	check("0 7 * * *", in(2026, 10, 31, 7, 0), in(2026, 11, 1, 7, 0))
	if d := in(2026, 11, 1, 7, 0).Sub(in(2026, 10, 31, 7, 0)); d != 25*time.Hour {
		t.Errorf("sanity: %v", d)
	}
	// Every result is strictly increasing over a whole DST year, minute by minute
	// for a frequent schedule (no repeats, no going backwards).
	s, _ = ParseCron("*/7 * * * *")
	cur := in(2026, 1, 1, 0, 0)
	for i := 0; i < 80000; i++ {
		n := s.Next(cur, ny)
		if !n.After(cur) || n.Sub(cur) > 2*time.Hour {
			t.Fatalf("step %d: Next(%s) = %s", i, cur, n)
		}
		cur = n
	}
}
