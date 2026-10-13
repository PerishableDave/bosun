// Package providers is a table of well-known email providers (Gmail, iCloud,
// Fastmail, Outlook.com, Yahoo) with their IMAP/SMTP/CalDAV endpoints and
// where to create an app password, plus a small autodiscovery fallback for
// unknown domains. It has no dependencies on the rest of Bosun, so both the
// CLI (the `bosun connect email` wizard) and integrations (for error hints)
// can import it.
package providers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Provider describes one email provider.
type Provider struct {
	// Name is the stable identifier used by --provider ("gmail", "icloud",
	// "fastmail", "outlook", "yahoo", "other").
	Name string
	// Label is the human-readable name ("iCloud Mail").
	Label string
	// Domains are the address domains that belong to the provider.
	Domains []string

	IMAPHost string
	IMAPPort int // 993 = implicit TLS
	SMTPHost string
	SMTPPort int // 465 = implicit TLS, otherwise STARTTLS

	// AppPasswordURL is where to create an app password.
	AppPasswordURL string
	// AppPasswordSteps explains how to create one, one step per entry.
	AppPasswordSteps []string
	// LoginHint is added to a failed login's error message (e.g. Gmail's
	// 2-Step Verification requirement).
	LoginHint string
	// Warning is shown before asking for the password (e.g. Microsoft's
	// basic-auth retirement).
	Warning string

	// CalDAVURL is the provider's CalDAV server, if any.
	CalDAVURL string
	// CalendarWithAppPassword reports whether the same app password works
	// for CalDAV, so the wizard can offer calendar setup at no extra cost.
	CalendarWithAppPassword bool
	// ICSSteps explains how to find a private (secret) iCalendar feed URL
	// for read-only calendar access, one step per entry. Empty if the
	// provider has none.
	ICSSteps []string
	// CalendarNote explains the calendar options when CalDAV with an app
	// password is not available.
	CalendarNote string
}

// SMTPStartTLS reports whether SMTP uses STARTTLS (any port but 465).
func (p Provider) SMTPStartTLS() bool { return p.SMTPPort != 465 }

// Known reports whether p is a real preset (not "other").
func (p Provider) Known() bool { return p.Name != "" && p.Name != Other }

// Other is the name of the catch-all provider that asks for the hosts.
const Other = "other"

var table = []Provider{
	{
		Name: "gmail", Label: "Gmail",
		Domains:  []string{"gmail.com", "googlemail.com"},
		IMAPHost: "imap.gmail.com", IMAPPort: 993,
		SMTPHost: "smtp.gmail.com", SMTPPort: 465,
		AppPasswordURL: "https://myaccount.google.com/apppasswords",
		AppPasswordSteps: []string{
			"Turn on 2-Step Verification for your Google account if it is not on yet (https://myaccount.google.com/signinoptions/twosv); app passwords require it.",
			"Open https://myaccount.google.com/apppasswords, enter a name such as \"Bosun\" and click Create.",
			"Copy the 16-character password (spaces do not matter).",
		},
		LoginHint: "Gmail only accepts an app password here, not your Google password, and app passwords need 2-Step Verification turned on " +
			"(https://myaccount.google.com/signinoptions/twosv). Create one at https://myaccount.google.com/apppasswords. " +
			"Accounts managed by a school or employer may have app passwords disabled.",
		ICSSteps: []string{
			"Open Google Calendar on the web (https://calendar.google.com) and click the gear icon → Settings.",
			"Under \"Settings for my calendars\" pick the calendar, then scroll to \"Integrate calendar\".",
			"Copy \"Secret address in iCal format\" (it starts with https://calendar.google.com/calendar/ical/ and ends in basic.ics).",
		},
		CalendarNote: "Google Calendar does not accept app passwords. Bosun can read it through its secret iCal address (read-only, no Google Cloud setup), " +
			"or read and write it through OAuth (`bosun connect google --scopes caldav`, which needs your own Google Cloud OAuth client; see the README).",
	},
	{
		Name: "icloud", Label: "iCloud Mail",
		Domains:  []string{"icloud.com", "me.com", "mac.com"},
		IMAPHost: "imap.mail.me.com", IMAPPort: 993,
		SMTPHost: "smtp.mail.me.com", SMTPPort: 587,
		AppPasswordURL: "https://appleid.apple.com",
		AppPasswordSteps: []string{
			"Open https://appleid.apple.com and sign in (two-factor authentication must be on).",
			"Go to Sign-In and Security → App-Specific Passwords and click + (Generate).",
			"Name it \"Bosun\" and copy the password (format xxxx-xxxx-xxxx-xxxx).",
		},
		LoginHint: "iCloud needs an app-specific password, not your Apple ID password: https://appleid.apple.com → Sign-In and Security → App-Specific Passwords. " +
			"Also check that iCloud Mail is turned on for the account.",
		CalDAVURL:               "https://caldav.icloud.com",
		CalendarWithAppPassword: true,
		ICSSteps: []string{
			"In the Calendar app (or icloud.com/calendar), share the calendar as a Public Calendar.",
			"Copy the link (it starts with webcal://); note that anyone with the link can read it.",
		},
	},
	{
		Name: "fastmail", Label: "Fastmail",
		Domains:  []string{"fastmail.com", "fastmail.fm", "fastmail.cn", "fastmail.to", "fastmail.net", "fastmail.org", "messagingengine.com", "sent.com", "fmail.co.uk"},
		IMAPHost: "imap.fastmail.com", IMAPPort: 993,
		SMTPHost: "smtp.fastmail.com", SMTPPort: 465,
		AppPasswordURL: "https://app.fastmail.com/settings/security/integrations",
		AppPasswordSteps: []string{
			"Open Fastmail → Settings → Privacy & Security → Integrations (https://app.fastmail.com/settings/security/integrations).",
			"Under App passwords click \"New app password\", choose access \"Mail, Contacts & Calendars\" (or IMAP/SMTP plus CalDAV).",
			"Name it \"Bosun\" and copy the password.",
		},
		LoginHint: "Fastmail needs an app password (Settings → Privacy & Security → Integrations → App passwords) with IMAP and SMTP access, " +
			"not your login password.",
		CalDAVURL:               "https://caldav.fastmail.com",
		CalendarWithAppPassword: true,
		ICSSteps: []string{
			"Fastmail → Settings → Calendars → pick the calendar → Sharing → enable \"Share with the public\" or \"iCal link\".",
			"Copy the iCalendar link.",
		},
	},
	{
		Name: "outlook", Label: "Outlook.com",
		Domains:  []string{"outlook.com", "hotmail.com", "live.com", "msn.com", "passport.com", "outlook.co.uk", "hotmail.co.uk", "live.co.uk", "hotmail.fr", "outlook.fr", "live.fr", "hotmail.de", "outlook.de", "outlook.es", "hotmail.es", "hotmail.it", "outlook.it"},
		IMAPHost: "outlook.office365.com", IMAPPort: 993,
		SMTPHost: "smtp-mail.outlook.com", SMTPPort: 587,
		AppPasswordURL: "https://account.live.com/proofs/AppPassword",
		AppPasswordSteps: []string{
			"Turn on two-step verification at https://account.microsoft.com/security (Advanced security options).",
			"On the same page, under \"App passwords\", choose \"Create a new app password\" (https://account.live.com/proofs/AppPassword).",
			"Copy the password.",
		},
		Warning: "Microsoft is phasing out password (\"basic\") sign-in for IMAP and SMTP on personal Outlook.com/Hotmail accounts. " +
			"An app password still works for some accounts; if the test below fails with a correct password, your account no longer allows it.",
		LoginHint: "Microsoft is retiring password (basic) authentication for Outlook.com IMAP/SMTP, so even a correct app password may be refused. " +
			"If it is, this account needs OAuth: link it with `bosun connect microsoft` for the Microsoft tools, or use a provider that still " +
			"offers app passwords (Fastmail, iCloud, Gmail) for Bosun's email integration.",
		ICSSteps: []string{
			"Open Outlook.com on the web → Settings (gear) → Calendar → Shared calendars.",
			"Under \"Publish a calendar\" pick the calendar and \"Can view all details\", click Publish.",
			"Copy the ICS link.",
		},
		CalendarNote: "Outlook.com calendars do not support CalDAV; Bosun can read them through a published ICS link (read-only).",
	},
	{
		Name: "yahoo", Label: "Yahoo Mail",
		Domains:  []string{"yahoo.com", "ymail.com", "rocketmail.com", "yahoo.co.uk", "yahoo.fr", "yahoo.de", "yahoo.es", "yahoo.it", "yahoo.ca", "yahoo.com.au", "yahoo.co.in", "yahoo.co.jp"},
		IMAPHost: "imap.mail.yahoo.com", IMAPPort: 993,
		SMTPHost: "smtp.mail.yahoo.com", SMTPPort: 465,
		AppPasswordURL: "https://login.yahoo.com/account/security",
		AppPasswordSteps: []string{
			"Open https://login.yahoo.com/account/security (Account Security).",
			"Click \"Generate app password\" (or \"Manage app passwords\"), name it \"Bosun\" and click Generate.",
			"Copy the password.",
		},
		LoginHint: "Yahoo needs an app password from Account Security (https://login.yahoo.com/account/security → Generate app password), " +
			"not your Yahoo password.",
		ICSSteps: []string{
			"Open Yahoo Calendar on the web, hover the calendar → the down arrow → Share.",
			"Under \"Links\", choose \"View only\" and copy the ICS link (\"To import into a calendar app\").",
		},
		CalendarNote: "Bosun can read a Yahoo calendar through its ICS link (read-only).",
	},
}

var other = Provider{
	Name: Other, Label: "Other provider", IMAPPort: 993, SMTPPort: 465,
	ICSSteps: []string{
		"Most calendar services can publish or share a calendar as an ICS (iCalendar) link; look for \"Publish\", \"Share\", \"Export\", \"iCal\" or \"ICS\" in the calendar's settings.",
	},
}

// All returns the known providers (without "other"), in a stable order.
func All() []Provider {
	out := make([]Provider, len(table))
	copy(out, table)
	return out
}

// Names returns the accepted --provider names, "other" last.
func Names() []string {
	out := make([]string, 0, len(table)+1)
	for _, p := range table {
		out = append(out, p.Name)
	}
	return append(out, Other)
}

// Lookup returns the provider named name (case-insensitive). Aliases such
// as "googlemail", "me", "hotmail" or "live" resolve via the domain table
// ("hotmail" → outlook).
func Lookup(name string) (Provider, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Provider{}, false
	}
	if name == Other {
		return other, true
	}
	for _, p := range table {
		if p.Name == name {
			return p, true
		}
	}
	for _, p := range table {
		for _, d := range p.Domains {
			if label, _, _ := strings.Cut(d, "."); label == name {
				return p, true
			}
		}
	}
	return Provider{}, false
}

// OtherProvider returns the catch-all provider.
func OtherProvider() Provider { return other }

// Domain returns the lower-cased domain of an email address ("" if none).
func Domain(addr string) string {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(addr[at+1:])), ".")
}

// ForDomain returns the provider owning domain.
func ForDomain(domain string) (Provider, bool) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	for _, p := range table {
		for _, d := range p.Domains {
			if d == domain {
				return p, true
			}
		}
	}
	return Provider{}, false
}

// ForEmail returns the provider for an email address, by its domain.
func ForEmail(addr string) (Provider, bool) { return ForDomain(Domain(addr)) }

// ForHost returns the provider whose IMAP or SMTP host is host, e.g. to add
// a provider-specific hint to a login error.
func ForHost(host string) (Provider, bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, p := range table {
		if host == p.IMAPHost || host == p.SMTPHost {
			return p, true
		}
	}
	return Provider{}, false
}

// --- autodiscovery ---

// Candidates are the hosts worth trying for an unknown domain, in order.
type Candidates struct {
	IMAP   []string // hosts; IMAP on port 993
	SMTP   []string // hosts; SMTP on 465, then 587
	CalDAV []string // URLs; discovery tries /.well-known/caldav (RFC 6764) at each
}

// CandidatesFor returns the conventional host names for domain:
// imap.<domain> / smtp.<domain>, then mail.<domain>, and the domain's
// /.well-known/caldav for calendars.
func CandidatesFor(domain string) Candidates {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		return Candidates{}
	}
	return Candidates{
		IMAP:   []string{"imap." + domain, "mail." + domain},
		SMTP:   []string{"smtp." + domain, "mail." + domain},
		CalDAV: []string{"https://" + domain + "/.well-known/caldav", "https://caldav." + domain + "/.well-known/caldav"},
	}
}

// DialFunc dials a TCP address; tests substitute it.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Discovered is the result of probing an unknown domain.
type Discovered struct {
	IMAPHost string // "" if none answered
	IMAPPort int
	SMTPHost string
	SMTPPort int
}

// ErrNotDiscovered means no candidate host accepted a connection.
var ErrNotDiscovered = errors.New("providers: no mail server found")

// Discover probes the candidate hosts of domain with a TCP connection
// (IMAP 993; SMTP 465 then 587) and returns the first that answer. It only
// checks reachability; logging in is the caller's job. dial nil uses a
// net.Dialer with a short timeout.
func Discover(ctx context.Context, domain string, dial DialFunc) (Discovered, error) {
	if dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second}
		dial = d.DialContext
	}
	c := CandidatesFor(domain)
	if len(c.IMAP) == 0 {
		return Discovered{}, fmt.Errorf("%w: empty domain", ErrNotDiscovered)
	}
	try := func(host string, port int) bool {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, err := dial(pctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	var d Discovered
	for _, h := range c.IMAP {
		if try(h, 993) {
			d.IMAPHost, d.IMAPPort = h, 993
			break
		}
	}
	for _, h := range c.SMTP {
		for _, p := range []int{465, 587} {
			if try(h, p) {
				d.SMTPHost, d.SMTPPort = h, p
				break
			}
		}
		if d.SMTPHost != "" {
			break
		}
	}
	if d.IMAPHost == "" && d.SMTPHost == "" {
		return d, fmt.Errorf("%w for %s (tried %s)", ErrNotDiscovered, domain, strings.Join(append(c.IMAP, c.SMTP...), ", "))
	}
	return d, nil
}

// --- ICS feeds ---

// NormalizeICSURL turns a calendar feed link into the https URL to fetch:
// webcal:// and webcals:// become https://; anything but https is refused,
// since the URL itself is the secret and must not travel in clear.
func NormalizeICSURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty calendar URL")
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "webcals://"):
		raw = "https://" + raw[len("webcals://"):]
	case strings.HasPrefix(lower, "webcal://"):
		raw = "https://" + raw[len("webcal://"):]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("not a valid calendar URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" {
		return nil, errors.New("calendar feed URLs must use https:// (or webcal://)")
	}
	if u.Hostname() == "" {
		return nil, errors.New("calendar URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("calendar URL must not contain a user name or password")
	}
	u.Fragment = ""
	return u, nil
}
