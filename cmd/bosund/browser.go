package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/integrations/browser"
	"github.com/perishabledave/bosun/internal/policy"
)

// browserControl adapts the browser integration to api.Browser for
// /v1/browser. It returns nil when the integration is not enabled.
func (d *daemon) browserControl() api.Browser {
	for _, in := range d.integrations {
		if b, ok := in.(*browser.Integration); ok {
			return browserAPI{b}
		}
	}
	return nil
}

type browserAPI struct{ b *browser.Integration }

func (x browserAPI) Status(_ context.Context, userID int64) api.BrowserStatus {
	s := x.b.UserStatus(userID)
	return api.BrowserStatus{Running: s.Running, Active: s.Active, URL: s.URL, Title: s.Title,
		Steps: s.Steps, Budget: s.Budget, LastAt: s.LastAt, HasScreenshot: s.HasScreenshot}
}

func (x browserAPI) Screenshot(ctx context.Context, userID int64) ([]byte, error) {
	b, err := x.b.LatestScreenshot(ctx, userID)
	if errors.Is(err, browser.ErrNoScreenshot) {
		return nil, api.ErrNoScreenshot
	}
	return b, err
}

func (x browserAPI) Stop(_ context.Context, userID int64) error {
	x.b.StopUser(userID)
	return nil
}

// allowScope returns the approval manager's AllowScope hook: it creates
// the user's scoped "allow" rule ("approve and always allow on <site>")
// and audits the change.
func allowScope(engine *policy.StoreEngine, aud audit.Logger) func(ctx context.Context, userID int64, c integration.Capability, scope string) error {
	return func(ctx context.Context, userID int64, c integration.Capability, scope string) error {
		if _, err := engine.SetScopedRule(ctx, userID, c, scope, policy.Allow); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]string{"scope": scope})
		_ = aud.Record(context.WithoutCancel(ctx), audit.Entry{UserID: userID, Kind: audit.KindConfig, Capability: c,
			Decision: string(policy.Allow), Notes: []string{"policy rule set from an approval (always allow on " + scope + ")"},
			Detail: detail})
		return nil
	}
}
