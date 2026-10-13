package main

// `bosun connect email` and `bosun connect calendar`: the easy path for
// linking accounts with app passwords and private calendar (ICS) links, no
// OAuth client required. Credentials are tested by the daemon
// (POST /v1/integrations/{name}/test) before they are stored through the
// credentials API; `bosun setup` runs the same wizard against the local
// vault before the daemon is running.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/providers"
)

// testFailedError is a credential the backend tried and rejected (wrong
// password, unreachable server, ...), as opposed to a failure to run the
// test at all.
type testFailedError struct{ msg string }

func (e *testFailedError) Error() string { return e.msg }

// accountBackend tests and stores credentials for the wizards.
type accountBackend interface {
	// test returns a *testFailedError when the credential does not work.
	test(ctx context.Context, integ string, req api.TestCredentialRequest) error
	store(ctx context.Context, integ, kind string, req api.PutCredentialRequest) error
}

// apiBackend talks to a running daemon as the logged-in user.
type apiBackend struct{ base, tok string }

func (b apiBackend) test(ctx context.Context, integ string, req api.TestCredentialRequest) error {
	var resp api.TestCredentialResponse
	err := callWith(ctx, slowHTTPClient, "POST", b.base+"/v1/integrations/"+url.PathEscape(integ)+"/test", b.tok, req, &resp)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return fmt.Errorf("the %s integration is not enabled on the daemon: add it under integrations: in bosun.yaml "+
				"(%s: {enabled: true}) and restart bosund (%w)", integ, integ, err)
		}
		return err
	}
	if !resp.OK {
		return &testFailedError{msg: resp.Error}
	}
	return nil
}

func (b apiBackend) store(ctx context.Context, integ, kind string, req api.PutCredentialRequest) error {
	var resp api.CredentialResponse
	return call(ctx, "PUT", credentialURL(b.base, integ, kind, api.CredentialScopeUser), b.tok, req, &resp)
}

// discoverDial is the dialer used to probe unknown domains; tests replace it.
var discoverDial providers.DialFunc

// wizard drives the interactive (or flag-driven) account setup.
type wizard struct {
	p           *prompter
	out         io.Writer
	be          accountBackend
	interactive bool
}

// emailOpts are the `bosun connect email` flags.
type emailOpts struct {
	provider, user, host, smtpHost string
	imapPort, smtpPort             int
	passwordStdin                  bool
	noCalendar                     bool
}

const maxAttempts = 3

func newConnectEmailCmd(g *globals) *cobra.Command {
	var o emailOpts
	cmd := &cobra.Command{
		Use:   "email",
		Short: "Link an email account with an app password (Gmail, iCloud, Fastmail, Outlook.com, Yahoo, others)",
		Long: "Asks for your address, works out the mail servers, shows where to create an app password,\n" +
			"tests the login (IMAP and SMTP) and only then stores it in your vault. For iCloud and Fastmail it\n" +
			"offers to set up the calendar with the same password; for Gmail and others it can add a read-only\n" +
			"calendar from its private iCal (ICS) link.\n\n" +
			"For scripts: bosun connect email --user you@fastmail.com --password-stdin < app-password.txt\n" +
			"(--provider and --host/--smtp-host/--imap-port/--smtp-port override the presets).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			w := &wizard{p: newPrompter(cmd.InOrStdin(), cmd.OutOrStdout()), out: cmd.OutOrStdout(),
				be: apiBackend{base: base, tok: tok}, interactive: !o.passwordStdin}
			return w.email(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.provider, "provider", "", "provider preset: "+strings.Join(providers.Names(), ", ")+" (default: from the address)")
	f.StringVar(&o.user, "user", "", "email address (the login)")
	f.StringVar(&o.host, "host", "", "IMAP server (overrides the preset)")
	f.StringVar(&o.smtpHost, "smtp-host", "", "SMTP server (overrides the preset)")
	f.IntVar(&o.imapPort, "imap-port", 0, "IMAP port (default 993; 143 uses STARTTLS)")
	f.IntVar(&o.smtpPort, "smtp-port", 0, "SMTP port (465 implicit TLS, otherwise STARTTLS; default from the preset)")
	f.BoolVar(&o.passwordStdin, "password-stdin", false, "read the app password from stdin and run without questions")
	f.BoolVar(&o.noCalendar, "no-calendar", false, "do not offer calendar setup")
	return cmd
}

func newConnectCalendarCmd(g *globals) *cobra.Command {
	var ics, name string
	cmd := &cobra.Command{
		Use:   "calendar [--ics [URL]] [--name NAME]",
		Short: "Add a read-only calendar from its private iCal (ICS) link",
		Long: "Stores a private calendar feed (the URL is the secret, so it is asked for with a hidden prompt\n" +
			"unless given) after a test fetch by the daemon. Any webcal:// link works. Examples:\n" +
			"  Google Calendar: Settings → your calendar → Integrate calendar → \"Secret address in iCal format\"\n" +
			"  Outlook.com:     Settings → Calendar → Shared calendars → Publish a calendar → ICS link\n" +
			"  iCloud:          share the calendar as a Public Calendar and copy the webcal:// link\n\n" +
			"Use --name for several feeds (e.g. --name family, --name work); they are merged in calendar.events.\n" +
			"For calendars you can edit, use `bosun connect email` (iCloud, Fastmail) or `bosun connect google --scopes caldav`.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw := ics
			if raw == "-" {
				raw = ""
			}
			if len(args) == 1 {
				if raw != "" {
					return errors.New("give the URL once (--ics=URL or as the argument)")
				}
				raw = args[0]
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			w := &wizard{p: newPrompter(cmd.InOrStdin(), cmd.OutOrStdout()), out: cmd.OutOrStdout(),
				be: apiBackend{base: base, tok: tok}, interactive: true}
			return w.ics(cmd.Context(), providers.OtherProvider(), name, cmd.Flags().Changed("name"), raw)
		},
	}
	cmd.Flags().StringVar(&ics, "ics", "", "private iCalendar feed URL (https:// or webcal://); prompted (hidden) if omitted")
	cmd.Flags().Lookup("ics").NoOptDefVal = "-"
	cmd.Flags().StringVar(&name, "name", "", "calendar name, e.g. family or work (stored as kind ics:<name>)")
	return cmd
}

// --- email ---

func (w *wizard) say(format string, args ...any) { fmt.Fprintf(w.out, format+"\n", args...) }

func (w *wizard) steps(steps []string) {
	for i, s := range steps {
		w.say("  %d. %s", i+1, s)
	}
}

func validAddress(s string) bool {
	at := strings.LastIndexByte(s, '@')
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t\r\n<>") && strings.Contains(s[at:], ".")
}

func smtpDesc(host string, port int) string {
	if port == 0 {
		return host + " (465, falling back to 587)"
	}
	mode := "STARTTLS"
	if port == 465 {
		mode = "TLS"
	}
	return fmt.Sprintf("%s:%d %s", host, port, mode)
}

// email runs the email wizard: address → provider → hosts → app password
// → test → store → calendar offer.
func (w *wizard) email(ctx context.Context, o emailOpts) error {
	addr := strings.TrimSpace(o.user)
	if addr == "" {
		if !w.interactive {
			return errors.New("--user is required with --password-stdin")
		}
		w.say("Connect an email account")
		w.say("")
		var err error
		for i := 0; ; i++ {
			if addr, err = w.p.line("Email address", ""); err != nil {
				return err
			}
			if validAddress(addr) {
				break
			}
			if i == maxAttempts-1 {
				return errors.New("no valid email address given")
			}
			w.say("That does not look like an email address.")
		}
	}
	if !validAddress(addr) {
		return fmt.Errorf("%q is not an email address", addr)
	}
	domain := providers.Domain(addr)

	prov, known := providers.ForDomain(domain)
	if o.provider != "" {
		p, ok := providers.Lookup(o.provider)
		if !ok {
			return fmt.Errorf("unknown provider %q (known: %s)", o.provider, strings.Join(providers.Names(), ", "))
		}
		prov, known = p, p.Known()
	}
	if !known {
		prov = providers.OtherProvider()
	}

	imapHost, smtpHost := prov.IMAPHost, prov.SMTPHost
	imapPort, smtpPort := prov.IMAPPort, prov.SMTPPort
	if !known {
		smtpPort = 0 // 465 with a 587 fallback
	}
	if o.host != "" {
		imapHost = o.host
	}
	if o.smtpHost != "" {
		smtpHost = o.smtpHost
	}
	if o.imapPort != 0 {
		imapPort = o.imapPort
	}
	if o.smtpPort != 0 {
		smtpPort = o.smtpPort
	}

	if known {
		w.say("Provider: %s (IMAP %s:%d, SMTP %s)", prov.Label, imapHost, imapPort, smtpDesc(smtpHost, smtpPort))
	} else if imapHost == "" || smtpHost == "" {
		if !w.interactive {
			return fmt.Errorf("unknown provider for %s: pass --host <IMAP server> and --smtp-host <SMTP server> (or --provider %s)",
				domain, strings.Join(providers.Names()[:len(providers.Names())-1], "|"))
		}
		w.say("%s is not a provider I know; looking for its mail servers...", domain)
		d, derr := providers.Discover(ctx, domain, discoverDial)
		defIMAP, defSMTP := d.IMAPHost, d.SMTPHost
		if derr != nil {
			w.say("Could not find them automatically; your provider's help pages list the IMAP and SMTP server names.")
			defIMAP, defSMTP = "imap."+domain, "smtp."+domain
		} else {
			w.say("Found IMAP %s and SMTP %s.", orNone(d.IMAPHost), orNone(d.SMTPHost))
			if d.SMTPPort != 0 && o.smtpPort == 0 {
				smtpPort = d.SMTPPort
			}
			if defIMAP == "" {
				defIMAP = "imap." + domain
			}
			if defSMTP == "" {
				defSMTP = "smtp." + domain
			}
		}
		var err error
		if imapHost == "" {
			if imapHost, err = w.p.line("IMAP server", defIMAP); err != nil {
				return err
			}
		}
		if smtpHost == "" {
			if smtpHost, err = w.p.line("SMTP server", defSMTP); err != nil {
				return err
			}
		}
	}
	if imapPort == 0 {
		imapPort = 993
	}

	if w.interactive {
		w.say("")
		if prov.Warning != "" {
			w.say("Note: %s", prov.Warning)
			w.say("")
		}
		if known {
			w.say("%s needs an app password (your normal password will not work). To create one:", prov.Label)
			w.steps(prov.AppPasswordSteps)
		} else {
			w.say("Use an app password if your provider offers them (look under security settings); otherwise your mail password.")
		}
		w.say("")
	}

	fields := map[string]string{"host": imapHost, "user": addr}
	if known {
		fields["provider"] = prov.Name
	}
	if imapPort != 993 {
		fields["imap_port"] = strconv.Itoa(imapPort)
	}
	if def := defaultSMTPHost(imapHost); smtpHost != def {
		fields["smtp_host"] = smtpHost
	}
	if smtpPort != 0 && (smtpPort != 465 || o.smtpPort != 0) { // 465 alone keeps the 587 fallback
		fields["smtp_port"] = strconv.Itoa(smtpPort)
	}

	var password string
	for attempt := 1; ; attempt++ {
		var err error
		if password, err = w.readPassword(fmt.Sprintf("App password for %s", addr)); err != nil {
			return err
		}
		if prov.Name == "gmail" {
			password = strings.Join(strings.Fields(password), "") // shown as four groups of four
		}
		w.say("Testing the connection (IMAP login and SMTP authentication)...")
		err = w.be.test(ctx, "email", api.TestCredentialRequest{Kind: "password", Secret: password, Fields: fields})
		if err == nil {
			w.say("Connection OK.")
			break
		}
		var tf *testFailedError
		if !errors.As(err, &tf) {
			return err
		}
		w.say("That did not work: %s", tf.msg)
		if !w.interactive || attempt == maxAttempts {
			return errors.New("email account not saved: the connection test failed")
		}
		again, err := w.p.yesNo("Try another password?", true)
		if err != nil || !again {
			return errors.New("email account not saved")
		}
	}
	if err := w.be.store(ctx, "email", "password", api.PutCredentialRequest{Secret: password, Fields: fields}); err != nil {
		return fmt.Errorf("store email credential: %w", err)
	}
	w.say("Saved. Bosun can now read and send mail as %s (by default, sending asks you first).", addr)

	if !w.interactive || o.noCalendar {
		return nil
	}
	return w.calendarOffer(ctx, prov, known, addr, password)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// defaultSMTPHost mirrors the email integration's default: the IMAP host
// with a leading "imap." replaced by "smtp.".
func defaultSMTPHost(imapHost string) string {
	if strings.HasPrefix(strings.ToLower(imapHost), "imap.") {
		return "smtp." + imapHost[len("imap."):]
	}
	return imapHost
}

// readPassword reads the app password: hidden prompt when interactive,
// all of stdin with --password-stdin.
func (w *wizard) readPassword(label string) (string, error) {
	var s string
	if w.interactive {
		var err error
		if s, err = w.p.password(label); err != nil {
			return "", err
		}
	} else {
		b, err := io.ReadAll(io.LimitReader(w.p.br, api.MaxCredentialSecret+2))
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		s = strings.TrimRight(string(b), "\r\n")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty password")
	}
	if len(s) > api.MaxCredentialSecret {
		return "", errors.New("password is too long")
	}
	return s, nil
}

// calendarOffer follows a saved email account with the matching calendar
// setup: CalDAV with the same app password where the provider allows it,
// otherwise a read-only ICS feed.
func (w *wizard) calendarOffer(ctx context.Context, prov providers.Provider, known bool, addr, password string) error {
	w.say("")
	if prov.CalendarWithAppPassword && prov.CalDAVURL != "" {
		ok, err := w.p.yesNo(fmt.Sprintf("%s Calendar works with the same app password. Set up your calendar too?", strings.TrimSuffix(prov.Label, " Mail")), true)
		if err != nil || !ok {
			w.say("Skipped. You can run `bosun connect email` again later.")
			return nil
		}
		fields := map[string]string{"url": prov.CalDAVURL, "user": addr}
		w.say("Testing CalDAV at %s...", prov.CalDAVURL)
		err = w.be.test(ctx, "calendar", api.TestCredentialRequest{Kind: "password", Secret: password, Fields: fields})
		if err != nil {
			var tf *testFailedError
			if errors.As(err, &tf) {
				w.say("Calendar not saved: %s", tf.msg)
			} else {
				w.say("Calendar not saved: %v", err)
			}
			return nil
		}
		if err := w.be.store(ctx, "calendar", "password", api.PutCredentialRequest{Secret: password, Fields: fields}); err != nil {
			return fmt.Errorf("store calendar credential: %w", err)
		}
		w.say("Saved. Bosun can now read your calendars and add or change events (by default, changes ask you first).")
		return nil
	}
	if len(prov.ICSSteps) == 0 {
		return nil
	}
	if prov.CalendarNote != "" {
		w.say("%s", prov.CalendarNote)
	}
	ok, err := w.p.yesNo("Add a read-only calendar from its private iCal (ICS) link now?", known)
	if err != nil || !ok {
		if prov.Name == "gmail" {
			w.say("Skipped. Later: `bosun connect calendar --ics` (read-only) or `bosun connect google --scopes caldav` (read/write, advanced).")
		} else {
			w.say("Skipped. You can add one later with `bosun connect calendar --ics`.")
		}
		return nil
	}
	def := prov.Name
	if prov.Name == "gmail" {
		def = "google"
	}
	if !known {
		def = ""
	}
	return w.ics(ctx, prov, def, false, "")
}

// --- ICS ---

var feedNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// ics stores a read-only calendar feed: show where to find the link, read
// it (hidden), name it, test it through the daemon, store it as "ics" or
// "ics:<name>".
func (w *wizard) ics(ctx context.Context, prov providers.Provider, name string, nameSet bool, raw string) error {
	if raw == "" {
		if len(prov.ICSSteps) > 0 && prov.Known() {
			w.say("To find your calendar's private link:")
			w.steps(prov.ICSSteps)
		} else {
			w.say("Paste your calendar's private iCal (ICS) link. Examples:")
			w.say("  Google Calendar: Settings → your calendar → Integrate calendar → \"Secret address in iCal format\"")
			w.say("  Outlook.com: Settings → Calendar → Shared calendars → Publish a calendar → ICS link")
			w.say("  iCloud: share the calendar as a Public Calendar and copy the webcal:// link")
		}
		w.say("The link works like a password: anyone who has it can read the calendar, so it is stored in your vault.")
	}
	var feed *url.URL
	for attempt := 1; ; attempt++ {
		var err error
		if raw == "" {
			if raw, err = w.p.password("Calendar link (input hidden)"); err != nil {
				return err
			}
		}
		feed, err = providers.NormalizeICSURL(raw)
		if err == nil {
			break
		}
		w.say("That link will not work: %v", err)
		if attempt == maxAttempts {
			return errors.New("calendar not saved")
		}
		raw = ""
	}
	if !nameSet {
		n, err := w.p.line("Name for this calendar (e.g. family, work)", name)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err == nil {
			name = n
		}
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name != "" && !feedNameRe.MatchString(name) {
		return fmt.Errorf("calendar name %q: use lowercase letters, digits, '.', '_' or '-'", name)
	}
	kind := "ics"
	if name != "" {
		kind = "ics:" + name
	}
	w.say("Testing the feed...")
	req := api.TestCredentialRequest{Kind: kind, Secret: feed.String()}
	if err := w.be.test(ctx, "calendar", req); err != nil {
		var tf *testFailedError
		if errors.As(err, &tf) {
			return fmt.Errorf("calendar not saved: %s", tf.msg)
		}
		return err
	}
	if err := w.be.store(ctx, "calendar", kind, api.PutCredentialRequest{Secret: feed.String()}); err != nil {
		return fmt.Errorf("store calendar feed: %w", err)
	}
	label := name
	if label == "" {
		label = "calendar"
	}
	w.say("Saved read-only calendar %q (credential calendar/%s). Bosun refreshes it every few minutes.", label, kind)
	return nil
}
