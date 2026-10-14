// Command bosun is the Bosun CLI, a thin client for the bosund daemon.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/tlsutil"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// globals holds persistent flag values for one CLI invocation.
type globals struct {
	configPath string
	server     string
	// caFile and fingerprint configure TLS verification (tlsclient.go).
	caFile      string
	fingerprint string
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
	root.PersistentFlags().StringVar(&g.server, "server", "", "daemon base URL, e.g. https://host:7777 (default: the server you logged in to, else derived from config daemon.listen)")
	root.PersistentFlags().StringVar(&g.caFile, "ca-file", "", "PEM file of certificates to trust for an https server")
	root.PersistentFlags().StringVar(&g.fingerprint, "insecure-fingerprint", "", "trust only the https server certificate with this SHA-256 fingerprint (for a self-signed daemon cert)")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if g.fingerprint != "" && g.caFile != "" {
			return fmt.Errorf("pass either --insecure-fingerprint or --ca-file, not both")
		}
		if g.fingerprint != "" {
			if _, err := tlsutil.ParseFingerprint(g.fingerprint); err != nil {
				return err
			}
		}
		cmd.SetContext(withClients(cmd.Context(), g))
		return nil
	}
	root.AddCommand(newSetupCmd(g), newStatusCmd(g), newLoginCmd(g), newChatCmd(g), newApprovalsCmd(g),
		newToolsCmd(g), newPolicyCmd(g), newMemoryCmd(g), newMCPCmd(g), newHooksCmd(g))
	root.AddCommand(newRoutinesCmd(g))
	root.AddCommand(newCredentialsCmd(g))
	root.AddCommand(newConnectCmd(g))
	root.AddCommand(newChannelsCmd(g))
	root.AddCommand(newUsersCmd(g), newAcceptInviteCmd(g), newPasswdCmd(g))
	root.AddCommand(newVersionCmd(g), newDoctorCmd(g), newServiceCmd(g), newBackupCmd(g), newRestoreCmd(g))
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
	if s := g.loadSavedServer(); s.Server != "" {
		return s.Server, nil
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return "", err
	}
	return cfg.BaseURL(), nil
}
