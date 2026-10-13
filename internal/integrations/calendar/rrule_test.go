package calendar

import (
	"strings"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

func TestExpandRecurrence(t *testing.T) {
	cases := []struct {
		name        string
		body        string // VEVENT(s) without BEGIN/END of the first
		from, to    string // dates in New York
		want        []string
		unsupported bool
	}{
		{
			name: "weekly BYDAY with COUNT",
			body: "DTSTART;TZID=America/New_York:20261005T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;COUNT=5\r\n",
			from: "2026-10-01", to: "2026-12-01",
			want: []string{"2026-10-05 09:00", "2026-10-07 09:00", "2026-10-09 09:00", "2026-10-12 09:00", "2026-10-14 09:00"},
		},
		{
			name: "weekly keeps wall clock across DST end",
			body: "DTSTART;TZID=America/New_York:20261026T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=WEEKLY\r\n",
			from: "2026-10-20", to: "2026-11-10",
			want: []string{"2026-10-26 09:00 EDT", "2026-11-02 09:00 EST", "2026-11-09 09:00 EST"},
		},
		{
			name: "weekly INTERVAL=2 with BYDAY and window mid-series",
			body: "DTSTART;TZID=America/New_York:20260105T180000\r\nRRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,TH\r\n",
			from: "2026-10-01", to: "2026-10-20",
			want: []string{"2026-10-01 18:00", "2026-10-12 18:00", "2026-10-15 18:00"}, // weeks 38 and 40 after DTSTART
		},
		{
			name: "daily UNTIL inclusive",
			body: "DTSTART:20261012T130000Z\r\nRRULE:FREQ=DAILY;UNTIL=20261015T130000Z\r\n",
			from: "2026-10-01", to: "2026-11-01",
			want: []string{"2026-10-12 09:00", "2026-10-13 09:00", "2026-10-14 09:00", "2026-10-15 09:00"},
		},
		{
			name: "daily COUNT with EXDATE (excluded instances still count)",
			body: "DTSTART;TZID=America/New_York:20261012T090000\r\nRRULE:FREQ=DAILY;COUNT=5\r\nEXDATE;TZID=America/New_York:20261014T090000,20261016T090000\r\n",
			from: "2026-10-01", to: "2026-11-01",
			want: []string{"2026-10-12 09:00", "2026-10-13 09:00", "2026-10-15 09:00"},
		},
		{
			name: "daily weekdays only",
			body: "DTSTART;TZID=America/New_York:20261009T080000\r\nRRULE:FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR\r\n",
			from: "2026-10-09", to: "2026-10-14",
			want: []string{"2026-10-09 08:00", "2026-10-12 08:00", "2026-10-13 08:00"},
		},
		{
			name: "monthly on the 31st skips short months",
			body: "DTSTART;TZID=America/New_York:20260131T100000\r\nRRULE:FREQ=MONTHLY;COUNT=4\r\n",
			from: "2026-01-01", to: "2027-01-01",
			want: []string{"2026-01-31 10:00", "2026-03-31 10:00", "2026-05-31 10:00", "2026-07-31 10:00"},
		},
		{
			name: "monthly second Tuesday",
			body: "DTSTART;TZID=America/New_York:20260908T190000\r\nRRULE:FREQ=MONTHLY;BYDAY=2TU\r\n",
			from: "2026-09-01", to: "2026-12-31",
			want: []string{"2026-09-08 19:00", "2026-10-13 19:00", "2026-11-10 19:00", "2026-12-08 19:00"},
		},
		{
			name: "monthly last Friday",
			body: "DTSTART;TZID=America/New_York:20260925T160000\r\nRRULE:FREQ=MONTHLY;BYDAY=-1FR;COUNT=3\r\n",
			from: "2026-09-01", to: "2027-09-01",
			want: []string{"2026-09-25 16:00", "2026-10-30 16:00", "2026-11-27 16:00"},
		},
		{
			name: "monthly last weekday via BYSETPOS",
			body: "DTSTART;TZID=America/New_York:20260930T120000\r\nRRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1\r\n",
			from: "2026-09-01", to: "2027-01-01",
			want: []string{"2026-09-30 12:00", "2026-10-30 12:00", "2026-11-30 12:00", "2026-12-31 12:00"},
		},
		{
			name: "monthly BYMONTHDAY list with UNTIL date",
			body: "DTSTART;TZID=America/New_York:20261001T090000\r\nRRULE:FREQ=MONTHLY;BYMONTHDAY=1,15;UNTIL=20261115\r\n",
			from: "2026-09-01", to: "2027-01-01",
			want: []string{"2026-10-01 09:00", "2026-10-15 09:00", "2026-11-01 09:00", "2026-11-15 09:00"},
		},
		{
			name: "yearly all-day Feb 29 only in leap years",
			body: "DTSTART;VALUE=DATE:20240229\r\nRRULE:FREQ=YEARLY\r\n",
			from: "2025-01-01", to: "2029-12-31",
			want: []string{"2028-02-29 00:00"},
		},
		{
			name: "yearly Thanksgiving",
			body: "DTSTART;VALUE=DATE:20241128\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=4TH\r\n",
			from: "2025-01-01", to: "2027-12-31",
			want: []string{"2025-11-27 00:00", "2026-11-26 00:00", "2027-11-25 00:00"},
		},
		{
			name: "all-day weekly with COUNT",
			body: "DTSTART;VALUE=DATE:20261012\r\nRRULE:FREQ=WEEKLY;COUNT=2\r\n",
			from: "2026-10-01", to: "2026-12-01",
			want: []string{"2026-10-12 00:00", "2026-10-19 00:00"},
		},
		{
			name: "RECURRENCE-ID override moves one instance",
			body: "DTSTART;TZID=America/New_York:20261012T090000\r\nDURATION:PT30M\r\nRRULE:FREQ=DAILY;COUNT=3\r\nSUMMARY:master\r\nEND:VEVENT\r\n" +
				"BEGIN:VEVENT\r\nUID:r\r\nRECURRENCE-ID;TZID=America/New_York:20261013T090000\r\n" +
				"DTSTART;TZID=America/New_York:20261013T150000\r\nDURATION:PT30M\r\nSUMMARY:moved\r\n",
			from: "2026-10-01", to: "2026-11-01",
			want: []string{"2026-10-12 09:00", "2026-10-13 15:00", "2026-10-14 09:00"},
		},
		{
			name: "cancelled override removes the instance",
			body: "DTSTART;TZID=America/New_York:20261012T090000\r\nRRULE:FREQ=DAILY;COUNT=3\r\nEND:VEVENT\r\n" +
				"BEGIN:VEVENT\r\nUID:r\r\nRECURRENCE-ID;TZID=America/New_York:20261013T090000\r\n" +
				"DTSTART;TZID=America/New_York:20261013T090000\r\nSTATUS:CANCELLED\r\n",
			from: "2026-10-01", to: "2026-11-01",
			want: []string{"2026-10-12 09:00", "2026-10-14 09:00"},
		},
		{
			name: "RDATE adds an instance",
			body: "DTSTART;TZID=America/New_York:20261012T090000\r\nRDATE;TZID=America/New_York:20261020T110000\r\n",
			from: "2026-10-01", to: "2026-11-01",
			want: []string{"2026-10-12 09:00", "2026-10-20 11:00"},
		},
		{
			name: "event spanning into the window is included",
			body: "DTSTART;TZID=America/New_York:20261012T220000\r\nDTEND;TZID=America/New_York:20261013T020000\r\nRRULE:FREQ=DAILY;COUNT=2\r\n",
			from: "2026-10-13", to: "2026-10-14",
			want: []string{"2026-10-12 22:00", "2026-10-13 22:00"},
		},
		{
			name: "unsupported FREQ yields first instance, flagged",
			body: "DTSTART;TZID=America/New_York:20261013T090000\r\nRRULE:FREQ=HOURLY;COUNT=5\r\n",
			from: "2026-10-01", to: "2026-11-01",
			want: []string{"2026-10-13 09:00"}, unsupported: true,
		},
		{
			name: "unsupported BYHOUR flagged",
			body: "DTSTART;TZID=America/New_York:20261013T090000\r\nRRULE:FREQ=DAILY;BYHOUR=9,17;COUNT=4\r\n",
			from: "2026-10-01", to: "2026-11-01",
			want: []string{"2026-10-13 09:00"}, unsupported: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := wrapCal("BEGIN:VEVENT\r\nUID:r\r\n" + tc.body + "END:VEVENT\r\n")
			cal, evs, err := parseEvents(data, nyc, nyc)
			if err != nil {
				t.Fatal(err)
			}
			from, _ := time.ParseInLocation("2006-01-02", tc.from, nyc)
			to, _ := time.ParseInLocation("2006-01-02", tc.to, nyc)
			insts := expand(evs, from, to, newTZCtx(cal, nyc, nyc), 1000)
			var got []string
			layout := "2006-01-02 15:04"
			if len(tc.want[0]) > len(layout) {
				layout += " MST"
			}
			for _, in := range insts {
				got = append(got, in.Start.In(nyc).Format(layout))
				if tc.unsupported != (in.Unsupported != "") {
					t.Errorf("unsupported = %q", in.Unsupported)
				}
			}
			sortStrings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestExpandLimitAndBadRule(t *testing.T) {
	_, evs, err := parseEvents(wrapCal("BEGIN:VEVENT\r\nUID:x\r\nDTSTART:20000101T000000Z\r\nRRULE:FREQ=DAILY\r\nEND:VEVENT\r\n"), nyc, nyc)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := expand(evs, from, from.AddDate(1, 0, 0), newTZCtx(nil, nyc, nyc), 50); len(got) != 50 {
		t.Errorf("limit: got %d", len(got))
	}
	for _, bad := range []string{"FREQ=WEEKLY;INTERVAL=0", "FREQ=WEEKLY;BYDAY=XX", "INTERVAL=2", "FREQ=MONTHLY;BYMONTHDAY=32", "FREQ=DAILY;BOGUS=1"} {
		if _, err := parseRRule(bad, from, false, newTZCtx(nil, nyc, nyc)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestWhenShorthandAcrossDST(t *testing.T) {
	// 2026-11-01: DST ends in New York (25-hour day).
	fallBack := time.Date(2026, 11, 1, 12, 0, 0, 0, nyc)
	// 2026-03-08: DST starts (23-hour day).
	springFwd := time.Date(2026, 3, 8, 12, 0, 0, 0, nyc)
	cases := []struct {
		now        time.Time
		from, to   string
		start, end string
		hours      float64
	}{
		{fallBack, "today", "", "2026-11-01T00:00:00-04:00", "2026-11-02T00:00:00-05:00", 25},
		{fallBack, "tomorrow", "", "2026-11-02T00:00:00-05:00", "2026-11-03T00:00:00-05:00", 24},
		{fallBack, "yesterday", "", "2026-10-31T00:00:00-04:00", "2026-11-01T00:00:00-04:00", 24},
		{fallBack, "this week", "", "2026-10-26T00:00:00-04:00", "2026-11-02T00:00:00-05:00", 7*24 + 1},
		{fallBack, "next week", "", "2026-11-02T00:00:00-05:00", "2026-11-09T00:00:00-05:00", 7 * 24},
		{fallBack, "today", "tomorrow", "2026-11-01T00:00:00-04:00", "2026-11-03T00:00:00-05:00", 49},
		{springFwd, "today", "", "2026-03-08T00:00:00-05:00", "2026-03-09T00:00:00-04:00", 23},
		{springFwd, "this weekend", "", "2026-03-07T00:00:00-05:00", "2026-03-09T00:00:00-04:00", 47},
		{springFwd, "this month", "", "2026-03-01T00:00:00-05:00", "2026-04-01T00:00:00-04:00", 31*24 - 1},
		{springFwd, "2026-03-08", "", "2026-03-08T00:00:00-05:00", "2026-03-09T00:00:00-04:00", 23},
		{springFwd, "2026-03-08 09:00", "2026-03-08T17:00:00-04:00", "2026-03-08T09:00:00-04:00", "2026-03-08T17:00:00-04:00", 8},
		{springFwd, "2026-03-08T06:00:00Z", "", "2026-03-08T01:00:00-05:00", "2026-03-09T02:00:00-04:00", 24},
		{springFwd, "", "", "2026-03-08T00:00:00-05:00", "2026-03-09T00:00:00-04:00", 23},
		{springFwd, "", "tomorrow", "2026-03-08T12:00:00-04:00", "2026-03-10T00:00:00-04:00", 36},
	}
	for _, tc := range cases {
		from, to, err := resolveRange(tc.from, tc.to, tc.now, nyc)
		if err != nil {
			t.Errorf("%q/%q: %v", tc.from, tc.to, err)
			continue
		}
		if from.Format(time.RFC3339) != tc.start || to.Format(time.RFC3339) != tc.end || to.Sub(from).Hours() != tc.hours {
			t.Errorf("%q/%q at %v: got %s – %s (%vh), want %s – %s (%vh)", tc.from, tc.to, tc.now,
				from.Format(time.RFC3339), to.Format(time.RFC3339), to.Sub(from).Hours(), tc.start, tc.end, tc.hours)
		}
	}
	for _, bad := range [][2]string{{"tomorrow", "today"}, {"2026-01-01", "2028-01-01"}, {"next tuesday", ""}} {
		if _, _, err := resolveRange(bad[0], bad[1], fallBack, nyc); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestGoogleScopeCheck(t *testing.T) {
	f := newFakeDAV(t).google("me@gmail.com", "tok")
	f.addCal("primary", "me@gmail.com", "", false)
	creds := newFakeCreds()
	creds.set(1, integration.Credential{Kind: KindGoogle, Secret: "tok", Fields: map[string]string{
		"email": "me@gmail.com", "scopes": "openid https://www.googleapis.com/auth/calendar.events"}})
	c := newTestCal(t, creds, f.srv.URL+"/caldav/v2/")
	_, err := call(t, c, userCtx(1), "calendar.calendars", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "--scopes caldav") {
		t.Errorf("narrow scope err = %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("server contacted despite missing scope: %v", f.log)
	}
	creds.set(1, integration.Credential{Kind: KindGoogle, Secret: "tok", Fields: map[string]string{
		"email": "me@gmail.com", "scopes": "openid https://www.googleapis.com/auth/calendar https://mail.google.com/"}})
	mustCall(t, c, userCtx(1), "calendar.calendars", map[string]any{})
}
