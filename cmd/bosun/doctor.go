package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/doctor"
	"github.com/perishabledave/bosun/internal/llm/providers"
	"github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/vault"
)

// doctorHTTP is used for local LLM reachability probes.
var doctorHTTP = &http.Client{Timeout: 8 * time.Second}

// errChecksFailed makes `bosun doctor` exit non-zero.
var errChecksFailed = errors.New("some checks failed")

func newDoctorCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation and the running daemon",
		Long: `doctor checks the config, the data dir, the vault master key, the daemon
(reachability and version), every LLM provider (key source and a cheap
reachability probe), integrations, channels, TLS, routines, free disk
space, and reverse-proxy settings, and prints ok / warn / fail per line.

When the daemon is up and you are logged in as an admin, the daemon runs
its side of the checks (GET /v1/doctor), so this also works against a
remote daemon. When it is down, the local checks run against the data dir.
It exits non-zero if any check fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			checks := runDoctor(cmd.Context(), g)
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(struct {
					Status doctor.Status  `json:"status"`
					Checks []doctor.Check `json:"checks"`
				}{doctor.Worst(checks), checks}); err != nil {
					return err
				}
			} else {
				printChecks(out, checks)
			}
			if doctor.Worst(checks) == doctor.Fail {
				return errChecksFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the checks as JSON")
	return cmd
}

func printChecks(out io.Writer, checks []doctor.Check) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCHECK\tDETAIL")
	n := map[doctor.Status]int{}
	for _, c := range checks {
		n[c.Status]++
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Status, c.Name, c.Detail)
	}
	tw.Flush()
	fmt.Fprintf(out, "\n%d ok, %d warn, %d fail\n", n[doctor.OK], n[doctor.Warn], n[doctor.Fail])
}

// runDoctor gathers the checks: config and daemon reachability from the
// CLI, then either the daemon's own report (admin) or the local checks.
func runDoctor(ctx context.Context, g *globals) []doctor.Check {
	var checks []doctor.Check
	cfg, err := g.loadConfig()
	if err != nil {
		cfg = nil
		if errors.Is(err, os.ErrNotExist) && g.server != "" {
			checks = append(checks, doctor.Check{Name: "config", Status: doctor.Warn, Detail: "no local " + g.configPath + "; checking the remote daemon only"})
		} else {
			checks = append(checks, doctor.Check{Name: "config", Status: doctor.Fail, Detail: err.Error()})
		}
	} else {
		checks = append(checks, doctor.Check{Name: "config", Status: doctor.OK, Detail: g.configPath + " loads and validates"})
	}

	// Daemon reachability and version.
	var remote *api.DoctorResponse
	var integrations *api.IntegrationListResponse
	reachable := false
	base, berr := g.baseURL()
	switch {
	case berr != nil:
		checks = append(checks, doctor.Check{Name: "daemon", Status: doctor.Fail, Detail: berr.Error()})
	default:
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		dv, err := daemonVersion(hctx, base)
		cancel()
		if err != nil {
			checks = append(checks, doctor.Check{Name: "daemon", Status: doctor.Fail,
				Detail: fmt.Sprintf("not reachable at %s (start it with `bosun service start` or `bosund`); running local checks only", base)})
			break
		}
		reachable = true
		c := doctor.Check{Name: "daemon", Status: doctor.OK, Detail: fmt.Sprintf("bosund %s reachable at %s", dv, base)}
		if dv != version {
			c.Status, c.Detail = doctor.Warn, fmt.Sprintf("bosund %s at %s, but this CLI is %s; upgrade the older one", dv, base, version)
		}
		checks = append(checks, c)
		if tok, err := g.readToken(); err == nil {
			var dr api.DoctorResponse
			if err := callWith(ctx, slowHTTPClient, "GET", base+"/v1/doctor", tok, nil, &dr); err == nil {
				remote = &dr
			} else {
				var il api.IntegrationListResponse
				if call(ctx, "GET", base+"/v1/integrations", tok, nil, &il) == nil {
					integrations = &il
				}
			}
		}
	}
	if remote != nil {
		return append(checks, remote.Checks...)
	}
	if cfg == nil {
		return checks
	}
	return append(checks, localChecks(ctx, cfg, reachable, integrations)...)
}

// localChecks are run by the CLI against the local data dir, when the
// daemon is down or the user is not an admin.
func localChecks(ctx context.Context, cfg *config.Config, reachable bool, il *api.IntegrationListResponse) []doctor.Check {
	checks := []doctor.Check{
		doctor.DataDir(cfg.Daemon.DataDir),
		doctor.MasterKey(ctx, cfg, detectKeychain),
	}
	checks = append(checks, localLLMChecks(ctx, cfg)...)

	switch {
	case il != nil:
		hasChannels := false
		sort.Slice(il.Integrations, func(i, j int) bool { return il.Integrations[i].Name < il.Integrations[j].Name })
		for _, in := range il.Integrations {
			if in.Name == providers.CredentialIntegration {
				continue
			}
			if in.Name == "channels" {
				hasChannels = true
			}
			checks = append(checks, doctor.Integration(in.Name, in.Status, in.Message, in.ToolCount))
		}
		if !hasChannels {
			checks = append(checks, doctor.Check{Name: "channels", Status: doctor.OK, Detail: "none enabled"})
		}
	case reachable:
		checks = append(checks, doctor.Check{Name: "integrations", Status: doctor.Warn, Detail: "log in (`bosun login`) to check integration and channel status"})
	default:
		checks = append(checks, doctor.Check{Name: "integrations", Status: doctor.Warn, Detail: "daemon not reachable; integration and channel status unknown"})
	}

	checks = append(checks, doctor.TLS(cfg, time.Now()))
	why := "daemon not reachable"
	if reachable {
		why = "needs an admin login (GET /v1/doctor)"
	}
	checks = append(checks, doctor.Check{Name: "routines", Status: doctor.Warn, Detail: "scheduler state unknown: " + why})
	if _, err := os.Stat(cfg.Daemon.DataDir); err == nil {
		checks = append(checks, doctor.Disk(cfg.Daemon.DataDir))
	}
	checks = append(checks, doctor.Proxy(cfg, 0, ""))
	return checks
}

// localLLMChecks reports each provider's key source without decrypting the
// vault: a vault key is only listed (the daemon probes with it), an
// environment key is probed directly, and Ollama is probed keyless.
func localLLMChecks(ctx context.Context, cfg *config.Config) []doctor.Check {
	inVault := map[string]bool{}
	if _, err := os.Stat(cfg.DBPath()); err == nil {
		if st, err := store.Open(ctx, cfg.DBPath()); err == nil {
			infos, err := vault.New(st, nil).List(ctx, nil, providers.CredentialIntegration)
			if err == nil {
				for _, in := range infos {
					inVault[in.Kind] = true
				}
			}
			st.Close()
		}
	}
	names := make([]string, 0, len(cfg.LLM.Providers))
	for n := range cfg.LLM.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	required := doctor.RequiredProviders(cfg)
	var out []doctor.Check
	for _, n := range names {
		pc := cfg.LLM.Providers[n]
		src := doctor.KeySource{Env: pc.APIKeyEnv}
		key := ""
		switch {
		case inVault[n]:
			src.Source = providers.SourceVault
		case pc.APIKeyEnv != "" && os.Getenv(pc.APIKeyEnv) != "":
			src.Source, key = providers.SourceEnv, os.Getenv(pc.APIKeyEnv)
		}
		probe := src.Source != providers.SourceVault
		c := doctor.LLM(ctx, doctorHTTP, n, pc, key, src, required[n], probe)
		if !probe && c.Status == doctor.OK {
			c.Detail = src.String() + "; the daemon probes it (`bosun doctor` while it runs, as an admin)"
		}
		if src.Source == providers.SourceEnv {
			c.Detail += " (note: bosund reads its own environment and $data_dir/env, not this shell's)"
		}
		out = append(out, c)
	}
	return out
}
