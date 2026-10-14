package calendar

import (
	"context"
	"fmt"

	"github.com/perishabledave/bosun/internal/integration"
)

var _ integration.CredentialTester = (*Integration)(nil)

// TestCredential implements integration.CredentialTester without storing
// anything: a "password" credential must log in and discover at least one
// calendar over CalDAV; an "ics" / "ics:<name>" credential must fetch (with
// the same https-only, private-address and size rules as the tools) and
// parse as an iCalendar feed.
func (c *Integration) TestCredential(ctx context.Context, cred integration.Credential) error {
	switch {
	case cred.Kind == KindPassword:
		if cred.Secret == "" {
			return fmt.Errorf("calendar: the password is empty")
		}
		acct, err := passwordAccount(cred)
		if err != nil {
			return err
		}
		cl := &client{hc: c.hc, acct: acct}
		cals, err := cl.discover(ctx)
		if err != nil {
			return err
		}
		if len(cals) == 0 {
			return fmt.Errorf("calendar: logged in to %s, but the account has no calendars", acct.start.Host)
		}
		return nil
	case isICSKind(cred.Kind):
		return c.testFeed(ctx, cred)
	default:
		return fmt.Errorf("calendar: credential kind %q cannot be tested (supported: %s, %s, %s:<name>)", cred.Kind, KindPassword, KindICS, KindICS)
	}
}
