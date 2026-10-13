package calendar

import (
	"strings"
	"testing"
	"time"
)

func wrapCal(vevent string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + vevent + "END:VCALENDAR\r\n"
}

func oneEvent(t *testing.T, vevent string, floating *time.Location) *Event {
	t.Helper()
	_, evs, err := parseEvents(wrapCal(vevent), floating, nyc)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	return evs[0]
}

func TestParseEventFields(t *testing.T) {
	utc := time.UTC
	berlin := mustLoc("Europe/Berlin")
	tokyo := mustLoc("Asia/Tokyo")
	cases := []struct {
		name      string
		body      string
		floating  *time.Location
		start     time.Time
		end       time.Time
		allDay    bool
		summary   string
		location  string
		desc      string
		startZone string
	}{
		{
			name:  "utc with dtend",
			body:  "DTSTART:20261013T140000Z\r\nDTEND:20261013T150000Z\r\n",
			start: time.Date(2026, 10, 13, 14, 0, 0, 0, utc), end: time.Date(2026, 10, 13, 15, 0, 0, 0, utc),
		},
		{
			name:  "tzid",
			body:  "DTSTART;TZID=America/New_York:20261013T090000\r\nDTEND;TZID=Europe/Berlin:20261013T160000\r\n",
			start: time.Date(2026, 10, 13, 13, 0, 0, 0, utc), end: time.Date(2026, 10, 13, 14, 0, 0, 0, utc),
			startZone: "America/New_York",
		},
		{
			name:  "quoted tzid",
			body:  "DTSTART;TZID=\"Europe/Berlin\":20261013T090000\r\nDURATION:PT30M\r\n",
			start: time.Date(2026, 10, 13, 9, 0, 0, 0, berlin), end: time.Date(2026, 10, 13, 9, 30, 0, 0, berlin),
		},
		{
			name:  "windows tzid",
			body:  "DTSTART;TZID=Eastern Standard Time:20261013T090000\r\nDURATION:PT1H\r\n",
			start: time.Date(2026, 10, 13, 9, 0, 0, 0, nyc), end: time.Date(2026, 10, 13, 10, 0, 0, 0, nyc),
		},
		{
			name:  "vendor-prefixed tzid",
			body:  "DTSTART;TZID=/mozilla.org/20050126_1/Asia/Tokyo:20261013T090000\r\n",
			start: time.Date(2026, 10, 13, 9, 0, 0, 0, tokyo), end: time.Date(2026, 10, 13, 9, 0, 0, 0, tokyo),
		},
		{
			name:  "unknown tzid falls back to daemon zone",
			body:  "DTSTART;TZID=Custom Zone 7:20261013T090000\r\nDURATION:P1DT2H\r\n",
			start: time.Date(2026, 10, 13, 9, 0, 0, 0, nyc), end: time.Date(2026, 10, 14, 11, 0, 0, 0, nyc),
		},
		{
			name:     "floating uses request zone",
			body:     "DTSTART:20261013T090000\r\nDTEND:20261013T100000\r\n",
			floating: tokyo,
			start:    time.Date(2026, 10, 13, 9, 0, 0, 0, tokyo), end: time.Date(2026, 10, 13, 10, 0, 0, 0, tokyo),
		},
		{
			name:  "all-day default one day",
			body:  "DTSTART;VALUE=DATE:20261013\r\n",
			start: time.Date(2026, 10, 13, 0, 0, 0, 0, nyc), end: time.Date(2026, 10, 14, 0, 0, 0, 0, nyc), allDay: true,
		},
		{
			name:  "all-day multi with DST change",
			body:  "DTSTART;VALUE=DATE:20261031\r\nDTEND;VALUE=DATE:20261103\r\n",
			start: time.Date(2026, 10, 31, 0, 0, 0, 0, nyc), end: time.Date(2026, 11, 3, 0, 0, 0, 0, nyc), allDay: true,
		},
		{
			name: "escapes and folding",
			body: "DTSTART:20261013T140000Z\r\nSUMMARY:Lunch\\, then\\; coffee \\\\ more\r\n" +
				"LOCATION:Café \r\n  Central\r\nDESCRIPTION:line1\\nline2\\Nline3 and a long folded\r\n\ttail\r\n",
			start: time.Date(2026, 10, 13, 14, 0, 0, 0, utc), end: time.Date(2026, 10, 13, 14, 0, 0, 0, utc),
			summary: `Lunch, then; coffee \ more`, location: "Café  Central", desc: "line1\nline2\nline3 and a long foldedtail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl := tc.floating
			if fl == nil {
				fl = nyc
			}
			e := oneEvent(t, "BEGIN:VEVENT\r\nUID:x\r\n"+tc.body+"END:VEVENT\r\n", fl)
			if !e.Start.Equal(tc.start) || !e.End.Equal(tc.end) || e.AllDay != tc.allDay {
				t.Errorf("got %v – %v allDay=%v, want %v – %v allDay=%v", e.Start, e.End, e.AllDay, tc.start, tc.end, tc.allDay)
			}
			if tc.summary != "" && e.Summary != tc.summary {
				t.Errorf("summary = %q", e.Summary)
			}
			if tc.location != "" && e.Location != tc.location {
				t.Errorf("location = %q", e.Location)
			}
			if tc.desc != "" && e.Description != tc.desc {
				t.Errorf("description = %q", e.Description)
			}
			if tc.startZone != "" && e.Start.Location().String() != tc.startZone {
				t.Errorf("zone = %s", e.Start.Location())
			}
		})
	}
}

func TestVTimezoneHint(t *testing.T) {
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Pacific Time (custom)\r\nX-LIC-LOCATION:America/Los_Angeles\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:19701101T020000\r\nTZOFFSETFROM:-0700\r\nTZOFFSETTO:-0800\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:v\r\nDTSTART;TZID=Pacific Time (custom):20261013T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	_, evs, err := parseEvents(data, nyc, nyc)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 13, 16, 0, 0, 0, time.UTC); !evs[0].Start.Equal(want) {
		t.Errorf("start = %v, want %v", evs[0].Start.UTC(), want)
	}
}

func TestAttendeesAndParams(t *testing.T) {
	e := oneEvent(t, "BEGIN:VEVENT\r\nUID:a\r\nDTSTART:20261013T140000Z\r\n"+
		"ORGANIZER;CN=\"Smith, Ann\":MAILTO:ann@example.com\r\n"+
		"ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;CN=Bob:mailto:bob@example.com\r\n"+
		"ATTENDEE;CN=\"Say ^'hi^'\":mailto:c@example.com\r\nEND:VEVENT\r\n", nyc)
	if e.Organizer == nil || e.Organizer.Name != "Smith, Ann" || e.Organizer.Email != "ann@example.com" {
		t.Errorf("organizer = %+v", e.Organizer)
	}
	if len(e.Attendees) != 2 || e.Attendees[0].PartStat != "NEEDS-ACTION" || e.Attendees[1].Name != `Say "hi"` {
		t.Errorf("attendees = %+v", e.Attendees)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{
		"",
		"BEGIN:VCALENDAR\r\n",
		"BEGIN:VCALENDAR\r\nEND:VEVENT\r\n",
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nnocolon\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nX;P=\"open:v\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		"SUMMARY:outside\r\n",
	} {
		if _, err := ParseICS(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, _, err := parseEvents(wrapCal("BEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\n"), nyc, nyc); err == nil {
		t.Error("VEVENT without DTSTART accepted")
	}
	if _, _, err := parseEvents(wrapCal("BEGIN:VEVENT\r\nUID:x\r\nDTSTART:2026-10-13\r\nEND:VEVENT\r\n"), nyc, nyc); err == nil {
		t.Error("bad date accepted")
	}
}

func TestEncodeFoldRoundTrip(t *testing.T) {
	long := strings.Repeat("Ünïcödé, text; with \\ escapes\nand newlines ", 8)
	ev := &Component{Name: "VEVENT"}
	ev.Set(&Property{Name: "UID", Value: "rt"})
	ev.Set(dateTimeProp("DTSTART", time.Date(2026, 10, 13, 9, 0, 0, 0, nyc), false))
	ev.SetText("SUMMARY", long)
	ev.Set(&Property{Name: "X-P", Params: []Param{{Name: "CN", Values: []string{"a:b", "c\"d"}}}, Value: "v"})
	cal := &Component{Name: "VCALENDAR", Children: []*Component{ev}}
	out := cal.Encode()
	for _, l := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
		if len(l) > 75 {
			t.Errorf("line longer than 75 octets (%d): %q", len(l), l)
		}
		if !utf8Valid(l) {
			t.Errorf("fold split a UTF-8 sequence: %q", l)
		}
	}
	if !strings.Contains(out, "DTSTART:20261013T130000Z") {
		t.Errorf("dtstart not UTC:\n%s", out)
	}
	back, err := ParseICS(out)
	if err != nil {
		t.Fatal(err)
	}
	be := back.ChildrenNamed("VEVENT")[0]
	if got := be.Text("SUMMARY"); got != long {
		t.Errorf("round trip summary:\n got %q\nwant %q", got, long)
	}
	if p := be.Prop("X-P"); p == nil || len(p.Params[0].Values) != 2 || p.Params[0].Values[0] != "a:b" || p.Params[0].Values[1] != `c"d` {
		t.Errorf("param round trip: %+v", p)
	}
	if back.Encode() != out {
		t.Error("re-encoding is not stable")
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"PT1H30M": 90 * time.Minute, "P1D": 24 * time.Hour, "P1W": 7 * 24 * time.Hour,
		"-PT15M": -15 * time.Minute, "P1DT1S": 24*time.Hour + time.Second,
	} {
		days, d, err := parseDuration(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := time.Duration(days)*24*time.Hour + d; got != want {
			t.Errorf("%s = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "P", "PT", "1H", "PTH", "P1H", "PT1D", "P1DT"} {
		if _, _, err := parseDuration(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got := formatDuration(26*time.Hour + 90*time.Second); got != "P1DT2H1M30S" {
		t.Errorf("format = %s", got)
	}
	if d, err := parseUserDuration("1h15m"); err != nil || d != 75*time.Minute {
		t.Errorf("user duration = %v %v", d, err)
	}
	if _, err := parseUserDuration("-1h"); err == nil {
		t.Error("negative accepted")
	}
}
