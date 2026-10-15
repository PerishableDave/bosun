package main

import (
	"fmt"
	"io"
	"net/url"
	"sort"
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
			if err := printRules(out, p.Rules); err != nil {
				return err
			}
			if len(p.IntegrationDefaults) > 0 {
				caps := make([]string, 0, len(p.IntegrationDefaults))
				for c := range p.IntegrationDefaults {
					caps = append(caps, c+"="+p.IntegrationDefaults[c])
				}
				sort.Strings(caps)
				fmt.Fprintf(out, "\nIntegration defaults (instead of the risk default when you have no rule): %s\n", strings.Join(caps, " "))
			}
			return nil
		},
	})
	var setScope, unsetScope string
	set := &cobra.Command{
		Use:   "set <capability> allow|ask|deny",
		Short: "Set your rule for a capability (optionally only on one site, with --scope)",
		Long: "Set your rule for a capability. With --scope the rule applies only to calls on that site\n" +
			"(and its subdomains), e.g. `bosun policy set browser.act allow --scope opentable.com`;\n" +
			"a scoped rule beats the capability-wide rule, which beats the integration's default\n" +
			"(e.g. browser.read asks), which beats the risk default.",
		Args: cobra.ExactArgs(2),
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
			if err := call(cmd.Context(), "PUT", base+"/v1/policy/"+url.PathEscape(args[0])+scopeQuery(setScope), tok,
				api.SetPolicyRuleRequest{Decision: decision}, &r); err != nil {
				return err
			}
			if r.Scope != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "%s on %s: %s\n", r.Capability, r.Scope, r.Decision)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", r.Capability, r.Decision)
			}
			return nil
		},
	}
	set.Flags().StringVar(&setScope, "scope", "", "limit the rule to one site, e.g. opentable.com")
	cmd.AddCommand(set)
	unset := &cobra.Command{
		Use:   "unset <capability>",
		Short: "Remove your rule for a capability (the risk default applies again)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "DELETE", base+"/v1/policy/"+url.PathEscape(args[0])+scopeQuery(unsetScope), tok, nil, nil); err != nil {
				return err
			}
			if unsetScope != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "%s on %s: rule removed\n", args[0], unsetScope)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: rule removed (risk default applies)\n", args[0])
			}
			return nil
		},
	}
	unset.Flags().StringVar(&unsetScope, "scope", "", "remove the rule for this site only")
	cmd.AddCommand(unset)
	return cmd
}

// printRules prints explicit rules as a table ("No explicit rules." if none).
func printRules(out io.Writer, rules []api.PolicyRuleResponse) error {
	if len(rules) == 0 {
		fmt.Fprintln(out, "No explicit rules.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	scoped := false
	for _, r := range rules {
		scoped = scoped || r.Scope != ""
	}
	if !scoped {
		fmt.Fprintln(tw, "CAPABILITY\tDECISION")
		for _, r := range rules {
			fmt.Fprintf(tw, "%s\t%s\n", r.Capability, r.Decision)
		}
		return tw.Flush()
	}
	fmt.Fprintln(tw, "CAPABILITY\tSITE\tDECISION")
	for _, r := range rules {
		scope := r.Scope
		if scope == "" {
			scope = "(any)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Capability, scope, r.Decision)
	}
	return tw.Flush()
}

// scopeQuery returns "?scope=<site>" or "".
func scopeQuery(scope string) string {
	if scope == "" {
		return ""
	}
	return "?scope=" + url.QueryEscape(scope)
}
