package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/integrations/mcp"
)

func newMCPCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Inspect the MCP bridge (external MCP servers configured under integrations.mcp)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show each configured MCP server's state and tool count",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.IntegrationListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/integrations", tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var in *api.IntegrationResponse
			for k := range resp.Integrations {
				if resp.Integrations[k].Name == "mcp" {
					in = &resp.Integrations[k]
				}
			}
			if in == nil {
				fmt.Fprintln(out, "The MCP bridge is not enabled. Add servers under `integrations: mcp: servers:` in bosun.yaml and restart bosund.")
				return nil
			}
			fmt.Fprintf(out, "MCP bridge %s: %s, %d tool(s)", in.Version, in.Status, in.ToolCount)
			if in.Message != "" {
				fmt.Fprintf(out, " (%s)", in.Message)
			}
			fmt.Fprintln(out)
			if in.Details == nil {
				fmt.Fprintln(out, "Per-server details are only shown to admins.")
				return nil
			}
			var servers []mcp.ServerStatus
			b, _ := json.Marshal(in.Details)
			if err := json.Unmarshal(b, &servers); err != nil {
				return fmt.Errorf("decode server status: %w", err)
			}
			if len(servers) == 0 {
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVER\tTRANSPORT\tSTATE\tTOOLS\tDETAIL")
			for _, s := range servers {
				detail := s.Error
				if detail == "" && s.ServerName != "" {
					detail = fmt.Sprintf("%s %s (protocol %s)", s.ServerName, s.ServerVersion, s.ProtocolVersion)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", s.Name, s.Transport, s.State, s.Tools, oneLineTrunc(detail, 80))
			}
			return tw.Flush()
		},
	})
	return cmd
}
