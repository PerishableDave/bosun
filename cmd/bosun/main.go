// Command bosun is the Bosun CLI, a thin client for the bosund daemon.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// globals holds persistent flag values for one CLI invocation.
type globals struct {
	configPath string
	server     string
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bosun:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "bosun",
		Short:         "Bosun: a capability-scoped personal agent",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&g.configPath, "config", config.DefaultPath(), "path to bosun.yaml (env BOSUN_CONFIG)")
	root.PersistentFlags().StringVar(&g.server, "server", "", "daemon base URL (default derived from config daemon.listen)")
	root.AddCommand(newSetupCmd(g), newStatusCmd(g), newLoginCmd(g), newChatCmd(g), newApprovalsCmd(g),
		newToolsCmd(g), newPolicyCmd(g), newMemoryCmd(g), newMCPCmd(g))
	return root
}

// loadConfig loads the config named by --config.
func (g *globals) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return nil, fmt.Errorf("%w (run `bosun setup` first)", err)
	}
	return cfg, nil
}

// baseURL returns --server or the URL derived from config.
func (g *globals) baseURL() (string, error) {
	if g.server != "" {
		return g.server, nil
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return "", err
	}
	return cfg.BaseURL(), nil
}
