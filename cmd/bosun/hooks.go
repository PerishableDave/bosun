package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/api"
)

func newHooksCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Inspect registered hooks (admin only; script hooks are configured in bosun.yaml)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List registered hooks by point, in run order",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.HookListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/hooks", tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			n := 0
			for _, hs := range resp.Hooks {
				n += len(hs)
			}
			if n == 0 {
				fmt.Fprintln(out, "No hooks registered.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "POINT\tNAME\tTYPE")
			for _, p := range api.HookPoints {
				for _, h := range resp.Hooks[p] {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", p, h.Name, h.Type)
				}
			}
			return tw.Flush()
		},
	})
	return cmd
}
