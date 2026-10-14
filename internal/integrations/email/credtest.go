package email

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/providers"
)

var _ integration.CredentialTester = (*Integration)(nil)

// loginRejectedError is an IMAP LOGIN the server answered with NO/BAD.
type loginRejectedError struct {
	host, user, text string
}

func (e *loginRejectedError) Error() string {
	return fmt.Sprintf("imap: login to %s as %s rejected: %s (check the app password / re-link the account)", e.host, e.user, e.text)
}

// TestCredential implements integration.CredentialTester for "password"
// credentials: it logs in to the IMAP server, then authenticates to the
// SMTP submission server (EHLO, STARTTLS where used, AUTH, QUIT) without
// sending anything. Nothing is stored. Errors say what to fix, with a
// provider-specific hint (Gmail's 2-Step Verification requirement,
// Microsoft's basic-auth retirement) when the host is a known provider's.
func (e *Integration) TestCredential(ctx context.Context, cred integration.Credential) error {
	if cred.Kind != KindPassword {
		return fmt.Errorf("email: only %q credentials can be tested (got %q); OAuth accounts are checked when you link them", KindPassword, cred.Kind)
	}
	a, err := passwordAccount(cred)
	if err != nil {
		return err
	}
	c, err := e.openIMAP(ctx, a)
	if err != nil {
		return explainIMAP(a, err)
	}
	c.logout()
	if err := e.checkSMTP(ctx, a); err != nil {
		return explainSMTP(a, err)
	}
	return nil
}

func hintFor(host string) string {
	if p, ok := providers.ForHost(host); ok && p.LoginHint != "" {
		return " " + p.LoginHint
	}
	return " Many providers require an app password (created in the account's security settings) instead of your normal password."
}

// isConnectError reports whether err happened before any protocol exchange.
func isConnectError(err error) bool {
	var ne net.Error
	var oe *net.OpError
	var de *net.DNSError
	return errors.As(err, &ne) || errors.As(err, &oe) || errors.As(err, &de) ||
		strings.Contains(err.Error(), ": connect ") || strings.Contains(err.Error(), ": tls ")
}

func explainIMAP(a *account, err error) error {
	var le *loginRejectedError
	if errors.As(err, &le) {
		return fmt.Errorf("wrong password or login refused: %s rejected the login for %s (server said: %s).%s",
			a.imapHost, a.user, oneLine(le.text, 200), hintFor(a.imapHost))
	}
	if isConnectError(err) {
		return fmt.Errorf("could not reach the IMAP server %s:%d: %v (check the server name and port)", a.imapHost, a.imapPort, err)
	}
	return fmt.Errorf("IMAP check failed on %s: %w", a.imapHost, err)
}

func explainSMTP(a *account, err error) error {
	host := a.imapHost
	if len(a.smtp) > 0 {
		host = a.smtp[0].Host
	}
	var se *SMTPError
	if errors.As(err, &se) && se.Command == "AUTH" {
		return fmt.Errorf("reading mail works, but the SMTP server %s refused the same login for sending (%d %s).%s",
			host, se.Code, oneLine(se.Text, 200), hintFor(host))
	}
	if isConnectError(err) {
		return fmt.Errorf("reading mail works, but the SMTP server %s could not be reached: %v (check the SMTP host and port)", host, err)
	}
	return fmt.Errorf("reading mail works, but the SMTP check on %s failed: %w", host, err)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
