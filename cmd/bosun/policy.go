package main

import (
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/api"
)

// authed returns the daemon base URL and the stored CLI token.
func (g *globals) authed() (base, token string, err error) {
	if base, err = g.baseURL(); err != nil {
		return "", "", err
	}
	if token, err = g.readToken(); err != nil {
		return "", "", fmt.Errorf("%w (run `bosun login`)", err)
	}
	return base, token, nil
}

func newToolsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "tools",
		Short: "List the tools the assistant can use, with their risk and your current policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.ToolListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/tools", tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(resp.Tools) == 0 {
				fmt.Fprintln(out, "No tools registered. Enable integrations under `integrations:` in bosun.yaml.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TOOL\tCAPABILITY\tRISK\tPOLICY\tDESCRIPTION")
			for _, t := range resp.Tools {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Capability, t.Risk, t.Decision, oneLineTrunc(t.Description, 60))
			}
			return tw.Flush()
		},
	}
}

func newPolicyCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Show or change your per-capability permission rules",
		Long: "Rules decide whether a capability is allowed, needs your approval (ask), or is denied.\n" +
			"A capability without a rule uses the default for its risk level.",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Show risk defaults and your explicit rules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var p api.PolicyResponse
			if err := call(cmd.Context(), "GET", base+"/v1/policy", tok, nil, &p); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Defaults: read=%s write=%s destructive=%s\n", p.Defaults.Read, p.Defaults.Write, p.Defaults.Destructive)
			if len(p.Rules) == 0 {
				fmt.Fprintln(out, "No explicit rules.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CAPABILITY\tDECISION")
			for _, r := range p.Rules {
				fmt.Fprintf(tw, "%s\t%s\n", r.Capability, r.Decision)
			}
			return tw.Flush()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "set <capability> allow|ask|deny",
		Short: "Set your rule for a capability",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			decision := strings.ToLower(args[1])
			if decision != "allow" && decision != "ask" && decision != "deny" {
				return fmt.Errorf("decision must be allow, ask, or deny, got %q", args[1])
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var r api.PolicyRuleResponse
			if err := call(cmd.Context(), "PUT", base+"/v1/policy/"+url.PathEscape(args[0]), tok,
				api.SetPolicyRuleRequest{Decision: decision}, &r); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", r.Capability, r.Decision)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "unset <capability>",
		Short: "Remove your rule for a capability (the risk default applies again)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "DELETE", base+"/v1/policy/"+url.PathEscape(args[0]), tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: rule removed (risk default applies)\n", args[0])
			return nil
		},
	})
	return cmd
}
