package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/integration"
)

func newCredentialsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "credentials",
		Aliases: []string{"creds"},
		Short:   "Manage the secrets integrations use (stored encrypted in the daemon's vault)",
		Long: "Credentials are per user, except for shared integrations (such as mcp and websearch),\n" +
			"which use one household-wide credential that only an admin can set (--shared).\n" +
			"Secrets are read from stdin or a hidden prompt, never from the command line.",
	}
	cmd.AddCommand(newCredentialsListCmd(g), newCredentialsSetCmd(g), newCredentialsRemoveCmd(g))
	return cmd
}

func newCredentialsListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your credentials and the shared ones (never shows secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.CredentialListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/credentials", tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(resp.Credentials) == 0 {
				fmt.Fprintln(out, "No credentials stored. Add one with `bosun credentials set <integration> <kind>`.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "INTEGRATION\tKIND\tSCOPE\tEXPIRES\tUPDATED")
			for _, c := range resp.Credentials {
				exp := "-"
				if c.Expiry != nil {
					exp = c.Expiry.Local().Format(time.DateTime)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Integration, c.Kind, c.Scope, exp, c.UpdatedAt.Local().Format(time.DateTime))
			}
			return tw.Flush()
		},
	}
}

func newCredentialsSetCmd(g *globals) *cobra.Command {
	var shared bool
	var fields []string
	var expires string
	cmd := &cobra.Command{
		Use:   "set <integration> <kind>",
		Short: "Store a credential (secret from stdin or a hidden prompt)",
		Long: "Store a credential for an integration. The secret is read from a hidden prompt when stdin\n" +
			"is a terminal, otherwise from stdin (e.g. `pass show github | bosun credentials set mcp bearer:github`).\n" +
			"It is never taken from a flag, so it does not end up in your shell history.\n\n" +
			"The kinds an integration accepts are those it declares (e.g. mcp: \"bearer\" or \"bearer:<suffix>\"\n" +
			"as named by a server's `credential:` setting).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, kind := args[0], args[1]
			req := api.PutCredentialRequest{}
			for _, f := range fields {
				k, v, ok := strings.Cut(f, "=")
				if !ok || k == "" {
					return fmt.Errorf("--field %q: want key=value", f)
				}
				if req.Fields == nil {
					req.Fields = map[string]string{}
				}
				req.Fields[k] = v
			}
			if expires != "" {
				t, err := time.Parse(time.RFC3339, expires)
				if err != nil {
					return fmt.Errorf("--expires: want RFC 3339, e.g. 2027-01-31T00:00:00Z: %w", err)
				}
				req.Expiry = &t
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			scope, err := credentialScope(cmd.Context(), cmd.ErrOrStderr(), base, tok, name, shared, true)
			if err != nil {
				return err
			}
			if req.Secret, err = readSecret(cmd, fmt.Sprintf("Secret for %s/%s", name, kind)); err != nil {
				return err
			}
			var resp api.CredentialResponse
			err = call(cmd.Context(), "PUT", credentialURL(base, name, kind, scope), tok, req, &resp)
			req.Secret = ""
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Stored %s/%s (%s).\n", resp.Integration, resp.Kind, resp.Scope)
			if name == "mcp" {
				fmt.Fprintln(cmd.OutOrStdout(), "MCP servers connect at startup: restart bosund for the change to take effect.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&shared, "shared", false, "store the household-wide credential (admin only; implied for shared integrations)")
	cmd.Flags().StringArrayVar(&fields, "field", nil, "extra non-secret field key=value (repeatable), e.g. --field username=dave")
	cmd.Flags().StringVar(&expires, "expires", "", "expiry time (RFC 3339)")
	return cmd
}

func newCredentialsRemoveCmd(g *globals) *cobra.Command {
	var shared bool
	cmd := &cobra.Command{
		Use:     "remove <integration> <kind>",
		Aliases: []string{"rm", "delete"},
		Short:   "Delete a stored credential",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, kind := args[0], args[1]
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			scope, err := credentialScope(cmd.Context(), cmd.ErrOrStderr(), base, tok, name, shared, false)
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "DELETE", credentialURL(base, name, kind, scope), tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s/%s (%s).\n", name, kind, scope)
			return nil
		},
	}
	cmd.Flags().BoolVar(&shared, "shared", false, "remove the household-wide credential (admin only; implied for shared integrations)")
	return cmd
}

func credentialURL(base, name, kind, scope string) string {
	return base + "/v1/credentials/" + url.PathEscape(name) + "/" + url.PathEscape(kind) + "?scope=" + scope
}

// credentialScope decides between "user" and "shared" for integration name:
// shared integrations always use the shared credential (implied --shared,
// with a notice on w); per-user integrations reject --shared. When the
// integration is not enabled, a write fails and a delete falls back to the
// flag, so stale credentials can still be removed.
func credentialScope(ctx context.Context, w io.Writer, base, tok, name string, shared, write bool) (string, error) {
	var resp api.IntegrationListResponse
	if err := call(ctx, "GET", base+"/v1/integrations", tok, nil, &resp); err != nil {
		return "", err
	}
	var names []string
	for _, in := range resp.Integrations {
		names = append(names, in.Name)
		if in.Name != name {
			continue
		}
		switch integration.Scope(in.Scope) {
		case integration.ScopeShared:
			if !shared {
				fmt.Fprintf(w, "%s is a shared integration: using the household-wide credential (--shared; admin only).\n", name)
			}
			return api.CredentialScopeShared, nil
		default:
			if shared {
				return "", fmt.Errorf("%s is a per-user integration: each user stores their own credential, so --shared does not apply", name)
			}
			return api.CredentialScopeUser, nil
		}
	}
	if write {
		sort.Strings(names)
		enabled := "none"
		if len(names) > 0 {
			enabled = strings.Join(names, ", ")
		}
		return "", fmt.Errorf("unknown integration %q (enabled: %s)", name, enabled)
	}
	if shared {
		return api.CredentialScopeShared, nil
	}
	return api.CredentialScopeUser, nil
}

// readSecret reads a secret with a hidden prompt when stdin is a terminal,
// otherwise all of stdin (one trailing newline is dropped, so `echo` and
// password managers work; multi-line secrets such as PEM keys are kept).
func readSecret(cmd *cobra.Command, label string) (string, error) {
	in := cmd.InOrStdin()
	var s string
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		var err error
		if s, err = newPrompter(in, cmd.ErrOrStderr()).password(label); err != nil {
			return "", err
		}
	} else {
		b, err := io.ReadAll(io.LimitReader(in, api.MaxCredentialSecret+2))
		if err != nil {
			return "", fmt.Errorf("read secret: %w", err)
		}
		s = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	if s == "" {
		return "", errors.New("empty secret (pipe it on stdin or type it at the prompt)")
	}
	if len(s) > api.MaxCredentialSecret {
		return "", errors.New("secret is too long")
	}
	return s, nil
}
