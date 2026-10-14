package main

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/api"
)

func newVersionCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version and, if reachable, the daemon's",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			printVersions(cmd.Context(), g, cmd.OutOrStdout())
			return nil
		},
	}
}

func printVersions(ctx context.Context, g *globals, out io.Writer) {
	fmt.Fprintf(out, "bosun   %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	base, err := g.baseURL()
	if err != nil {
		fmt.Fprintf(out, "bosund  unknown (%v)\n", err)
		return
	}
	dv, err := daemonVersion(ctx, base)
	if err != nil {
		fmt.Fprintf(out, "bosund  not reachable at %s\n", base)
		return
	}
	fmt.Fprintf(out, "bosund  %s at %s\n", dv, base)
	if dv != version {
		fmt.Fprintln(out, "note: the CLI and daemon versions differ; upgrade the older one")
	}
}

// daemonVersion asks GET /v1/health for the daemon's version.
func daemonVersion(ctx context.Context, base string) (string, error) {
	var h api.HealthResponse
	if err := call(ctx, "GET", base+"/v1/health", "", nil, &h); err != nil {
		return "", err
	}
	return h.Version, nil
}
