package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the configuration file",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Load and validate the config file (--config) without contacting the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := g.loadConfig(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %s loads and validates\n", g.configPath)
			return nil
		},
	})
	return cmd
}
