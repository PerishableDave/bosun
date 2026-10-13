package providers

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestLookupByDomainAndName(t *testing.T) {
	for addr, want := range map[string]string{
		"dave@gmail.com":      "gmail",
		"Dave@GoogleMail.com": "gmail",
		"x@icloud.com":        "icloud",
		"x@me.com":            "icloud",
		"x@mac.com":           "icloud",
		"x@fastmail.com":      "fastmail",
		"x@outlook.com":       "outlook",
		"x@hotmail.com":       "outlook",
		"x@live.com":          "outlook",
		"x@yahoo.com":         "yahoo",
		"x@example.org":       "",
		"no-at-sign":          "",
		"trailing@gmail.com.": "gmail",
	} {
		p, ok := ForEmail(addr)
		if got := p.Name; got != want || ok != (want != "") {
			t.Errorf("ForEmail(%q) = %q, %v; want %q", addr, got, ok, want)
		}
	}
	for name, want := range map[string]string{
		"gmail": "gmail", "GMAIL": "gmail", "googlemail": "gmail", "icloud": "icloud", "me": "icloud",
		"fastmail": "fastmail", "outlook": "outlook", "hotmail": "outlook", "live": "outlook", "yahoo": "yahoo", "other": "other",
	} {
		p, ok := Lookup(name)
		if !ok || p.Name != want {
			t.Errorf("Lookup(%q) = %q, %v; want %q", name, p.Name, ok, want)
		}
	}
	if _, ok := Lookup("aol-ish"); ok {
		t.Error("unknown provider found")
	}
	if _, ok := Lookup(""); ok {
		t.Error("empty name found")
	}
}

func TestPresets(t *testing.T) {
	type want struct {
		imap        string
		imapPort    int
		smtp        string
		smtpPort    int
		starttls    bool
		caldav      string
		calWithPass bool
		url         string
	}
	cases := map[string]want{
		"gmail":    {"imap.gmail.com", 993, "smtp.gmail.com", 465, false, "", false, "https://myaccount.google.com/apppasswords"},
		"icloud":   {"imap.mail.me.com", 993, "smtp.mail.me.com", 587, true, "https://caldav.icloud.com", true, "https://appleid.apple.com"},
		"fastmail": {"imap.fastmail.com", 993, "smtp.fastmail.com", 465, false, "https://caldav.fastmail.com", true, ""},
		"outlook":  {"outlook.office365.com", 993, "smtp-mail.outlook.com", 587, true, "", false, ""},
		"yahoo":    {"imap.mail.yahoo.com", 993, "smtp.mail.yahoo.com", 465, false, "", false, ""},
	}
	for name, w := range cases {
		p, _ := Lookup(name)
		if p.IMAPHost != w.imap || p.IMAPPort != w.imapPort || p.SMTPHost != w.smtp || p.SMTPPort != w.smtpPort ||
			p.SMTPStartTLS() != w.starttls || p.CalDAVURL != w.caldav || p.CalendarWithAppPassword != w.calWithPass {
			t.Errorf("%s: %+v", name, p)
		}
		if w.url != "" && p.AppPasswordURL != w.url {
			t.Errorf("%s: app password URL %q", name, p.AppPasswordURL)
		}
		if p.AppPasswordURL == "" || len(p.AppPasswordSteps) == 0 || p.LoginHint == "" || len(p.ICSSteps) == 0 {
			t.Errorf("%s: missing instructions: %+v", name, p)
		}
		if !p.CalendarWithAppPassword && p.CalendarNote == "" {
			t.Errorf("%s: no calendar note", name)
		}
	}
	g, _ := Lookup("gmail")
	if !strings.Contains(g.LoginHint, "2-Step Verification") {
		t.Errorf("gmail hint: %s", g.LoginHint)
	}
	o, _ := Lookup("outlook")
	if !strings.Contains(o.Warning, "basic") || !strings.Contains(o.LoginHint, "OAuth") {
		t.Errorf("outlook warning/hint: %s / %s", o.Warning, o.LoginHint)
	}
	if p, ok := ForHost("IMAP.gmail.com:993"); !ok || p.Name != "gmail" {
		t.Errorf("ForHost gmail = %v %v", p.Name, ok)
	}
	if p, ok := ForHost("smtp-mail.outlook.com"); !ok || p.Name != "outlook" {
		t.Errorf("ForHost outlook = %v %v", p.Name, ok)
	}
	if _, ok := ForHost("mail.example.org"); ok {
		t.Error("ForHost unknown")
	}
	if names := Names(); names[len(names)-1] != Other || len(names) != len(All())+1 {
		t.Errorf("Names = %v", names)
	}
}

func TestCandidatesAndDiscover(t *testing.T) {
	c := CandidatesFor("Example.ORG")
	if strings.Join(c.IMAP, ",") != "imap.example.org,mail.example.org" ||
		strings.Join(c.SMTP, ",") != "smtp.example.org,mail.example.org" ||
		c.CalDAV[0] != "https://example.org/.well-known/caldav" {
		t.Errorf("candidates = %+v", c)
	}

	var mu sync.Mutex
	var tried []string
	open := map[string]bool{"mail.example.org:993": true, "mail.example.org:587": true}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		tried = append(tried, addr)
		mu.Unlock()
		if !open[addr] {
			return nil, errors.New("refused")
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	d, err := Discover(context.Background(), "example.org", dial)
	if err != nil {
		t.Fatal(err)
	}
	if d.IMAPHost != "mail.example.org" || d.IMAPPort != 993 || d.SMTPHost != "mail.example.org" || d.SMTPPort != 587 {
		t.Errorf("discovered %+v", d)
	}
	want := "imap.example.org:993,mail.example.org:993,smtp.example.org:465,smtp.example.org:587,mail.example.org:465,mail.example.org:587"
	if strings.Join(tried, ",") != want {
		t.Errorf("tried %v", tried)
	}

	open = map[string]bool{}
	if _, err := Discover(context.Background(), "nothing.example", dial); !errors.Is(err, ErrNotDiscovered) {
		t.Errorf("err = %v", err)
	}
}

func TestNormalizeICSURL(t *testing.T) {
	for in, want := range map[string]string{
		"webcal://p01-caldav.icloud.com/published/2/abc":                      "https://p01-caldav.icloud.com/published/2/abc",
		"WEBCALS://example.com/a.ics":                                         "https://example.com/a.ics",
		" https://calendar.google.com/calendar/ical/x/private-y/basic.ics#x ": "https://calendar.google.com/calendar/ical/x/private-y/basic.ics",
	} {
		u, err := NormalizeICSURL(in)
		if err != nil || u.String() != want {
			t.Errorf("NormalizeICSURL(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "http://example.com/a.ics", "ftp://x/y", "https://", "https://u:p@example.com/a.ics", "::"} {
		if _, err := NormalizeICSURL(bad); err == nil {
			t.Errorf("NormalizeICSURL(%q) accepted", bad)
		}
	}
}
