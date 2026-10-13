package main

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/oauth"
)

// openBrowser opens url in the user's browser. Tests replace it.
var openBrowser = func(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // reap; the opener's exit status is irrelevant
	return nil
}

// connectPollInterval is how often `bosun connect` polls the flow status.
var connectPollInterval = 2 * time.Second

func newConnectCmd(g *globals) *cobra.Command {
	var scopes string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "connect <provider>",
		Short: "Link your Google or Microsoft account so integrations can use it (OAuth2)",
		Long: "Opens the provider's sign-in page in your browser; after you approve, the provider\n" +
			"redirects to bosund, which stores the tokens encrypted in your vault and refreshes them\n" +
			"as needed. Integrations that need the account (email, calendar, ...) then use it.\n\n" +
			"--scopes picks scope bundles: google: email (Gmail read/send/modify), calendar, tasks,\n" +
			"drive (read-only), imap (full Gmail for IMAP/SMTP), caldav (full calendar for CalDAV).\n" +
			"Default: email,calendar. Connecting again replaces the stored account for that provider.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := args[0]
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			req := api.OAuthStartRequest{}
			for _, s := range strings.Split(scopes, ",") {
				if s = strings.TrimSpace(s); s != "" {
					req.Scopes = append(req.Scopes, s)
				}
			}
			var start oauth.StartResult
			if err := call(cmd.Context(), "POST", base+"/v1/oauth/"+url.PathEscape(provider)+"/start", tok, req, &start); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Open this URL in a browser on this machine to connect your %s account:\n\n  %s\n\n", provider, start.AuthURL)
			if !noBrowser {
				if err := openBrowser(start.AuthURL); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "(could not open a browser: %v)\n", err)
				}
			}
			fmt.Fprintf(out, "Waiting for you to approve (redirect: %s)...\n", start.RedirectURL)
			acct, err := waitForConnect(cmd.Context(), base, tok, provider, start)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Connected %s account %s.\nScopes: %s\n", provider, acct.Email, strings.Join(acct.Scopes, " "))
			return nil
		},
	}
	cmd.Flags().StringVar(&scopes, "scopes", "", "comma-separated scope bundles (default: email,calendar)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "only print the URL (headless use)")
	cmd.AddCommand(newConnectListCmd(g), newConnectRemoveCmd(g))
	return cmd
}

func waitForConnect(ctx context.Context, base, tok, provider string, start oauth.StartResult) (*oauth.Account, error) {
	deadline := time.Until(start.ExpiresAt) + 5*time.Second
	if deadline <= 0 || deadline > oauth.StateTTL+5*time.Second {
		deadline = oauth.StateTTL + 5*time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	statusURL := base + "/v1/oauth/" + url.PathEscape(provider) + "/status?state=" + url.QueryEscape(start.State)
	t := time.NewTicker(connectPollInterval)
	defer t.Stop()
	for {
		var st oauth.FlowStatus
		if err := call(ctx, "GET", statusURL, tok, nil, &st); err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("gave up waiting for the authorization: %w", ctx.Err())
			}
			return nil, err
		}
		switch st.Status {
		case oauth.StatusComplete:
			if st.Account == nil {
				return nil, fmt.Errorf("authorization complete but no account reported")
			}
			return st.Account, nil
		case oauth.StatusError:
			return nil, fmt.Errorf("connect %s failed: %s", provider, st.Error)
		case oauth.StatusExpired:
			return nil, fmt.Errorf("connect %s: %s; run `bosun connect %s` again", provider, st.Error, provider)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gave up waiting for the authorization: %w", ctx.Err())
		case <-t.C:
		}
	}
}

func newConnectListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your linked accounts (never shows tokens)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.OAuthListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/oauth", tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(resp.Accounts) == 0 {
				var configured []string
				for _, p := range resp.Providers {
					if p.Configured {
						configured = append(configured, p.Name)
					}
				}
				if len(configured) == 0 {
					fmt.Fprintln(out, "No linked accounts, and no OAuth provider is configured (see oauth: in bosun.yaml).")
				} else {
					fmt.Fprintf(out, "No linked accounts. Connect one with `bosun connect <provider>` (configured: %s).\n", strings.Join(configured, ", "))
				}
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PROVIDER\tACCOUNT\tLINKED\tSCOPES")
			for _, a := range resp.Accounts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Provider, a.Email, a.LinkedAt.Local().Format(time.DateTime), strings.Join(a.Scopes, " "))
			}
			return tw.Flush()
		},
	}
}

func newConnectRemoveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <provider>",
		Aliases: []string{"rm", "disconnect"},
		Short:   "Unlink an account: revoke the grant at the provider (best effort) and delete the tokens",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "DELETE", base+"/v1/oauth/"+url.PathEscape(args[0]), tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Disconnected %s.\n", args[0])
			return nil
		},
	}
}
