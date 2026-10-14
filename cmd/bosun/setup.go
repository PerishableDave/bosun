package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/auth"
	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/store"
)

func newSetupCmd(g *globals) *cobra.Command {
	var force, lan, regenCert, noConnect bool
	var listen, dataDir, admin string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Create bosun.yaml, the data directory, and the first admin user",
		Long: `setup writes bosun.yaml with sensible defaults (unless it already exists),
creates the data directory with mode 0700, initialises the database, and
creates the first admin user if none exists. It is safe to re-run.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup(cmd.Context(), g, newPrompter(cmd.InOrStdin(), cmd.OutOrStdout()), setupOpts{
				force: force, listen: listen, dataDir: dataDir, admin: admin,
				listenSet: cmd.Flags().Changed("listen"), dataDirSet: cmd.Flags().Changed("data-dir"),
				lan: lan, lanSet: cmd.Flags().Changed("lan"), regenCert: regenCert, noConnect: noConnect,
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing bosun.yaml")
	cmd.Flags().StringVar(&listen, "listen", config.DefaultListen, "daemon listen address")
	cmd.Flags().StringVar(&dataDir, "data-dir", config.DefaultDataDir, "data directory")
	cmd.Flags().StringVar(&admin, "admin", "", "admin username (prompted if empty)")
	cmd.Flags().BoolVar(&lan, "lan", false, "listen on the local network ("+lanListen+") over HTTPS with a self-signed certificate")
	cmd.Flags().BoolVar(&noConnect, "no-connect", false, "do not offer to connect an email account and calendar")
	cmd.Flags().BoolVar(&regenCert, "regenerate-cert", false, "replace the generated self-signed certificate (e.g. after the host's IP changed)")
	return cmd
}

type setupOpts struct {
	force                 bool
	listen, dataDir       string
	listenSet, dataDirSet bool
	admin                 string
	lan, lanSet           bool
	regenCert             bool
	noConnect             bool
}

func runSetup(ctx context.Context, g *globals, p *prompter, o setupOpts) error {
	out := p.out
	fmt.Fprintln(out, "Bosun setup")
	fmt.Fprintln(out)

	_, statErr := os.Stat(g.configPath)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("check config: %w", statErr)
	}

	if exists && !o.force && o.lan {
		if err := enableLANInConfig(g.configPath, o); err != nil {
			return err
		}
		fmt.Fprintf(out, "Updated %s for LAN access over HTTPS.\n", g.configPath)
	} else if exists && !o.force {
		fmt.Fprintf(out, "Using existing config %s (pass --force to overwrite).\n", g.configPath)
	} else {
		cfg := config.Default()
		// websearch.fetch (read risk, SSRF-guarded) works without any backend;
		// websearch.search needs base_url pointing at a SearXNG instance.
		enabled := true
		cfg.Integrations["websearch"] = config.IntegrationConfig{Enabled: &enabled}
		// email and calendar are harmless without credentials (their tools
		// answer "not configured" until `bosun connect email` / `connect
		// calendar` links an account), so they are on by default.
		cfg.Integrations["email"] = config.IntegrationConfig{Enabled: &enabled}
		cfg.Integrations["calendar"] = config.IntegrationConfig{Enabled: &enabled}
		var err error
		lan := o.lan
		if !o.lanSet && !o.listenSet {
			if lan, err = p.yesNo("Allow other devices on your network to connect? (enables HTTPS with a self-signed certificate)", false); err != nil {
				return err
			}
		}
		listen := o.listen
		if lan && !o.listenSet {
			listen = lanListen
		}
		if cfg.Daemon.Listen, err = answer(p, "Listen address", listen, o.listenSet || lan); err != nil {
			return err
		}
		if cfg.Daemon.DataDir, err = answer(p, "Data directory", o.dataDir, o.dataDirSet); err != nil {
			return err
		}
		if lan {
			setLANTLS(cfg)
		}
		// Validate a copy with paths expanded before writing anything.
		check := *cfg
		if check.Daemon.DataDir, err = config.ExpandPath(cfg.Daemon.DataDir); err != nil {
			return err
		}
		if err := check.Validate(); err != nil {
			return fmt.Errorf("invalid settings: %w", err)
		}
		if err := config.Save(g.configPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote %s\n", g.configPath)
	}

	cfg, err := config.Load(g.configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Daemon.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(cfg.Daemon.DataDir, 0o700); err != nil {
		return fmt.Errorf("chmod data dir: %w", err)
	}
	fmt.Fprintf(out, "Data directory %s (mode 0700)\n", cfg.Daemon.DataDir)
	if cfg.Daemon.TLS.Enabled {
		if err := ensureTLSCert(out, cfg, o.regenCert); err != nil {
			return err
		}
	}

	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	authSvc, err := auth.New(st, auth.Options{})
	if err != nil {
		return err
	}
	has, err := authSvc.HasUsers(ctx)
	if err != nil {
		return err
	}
	if has {
		fmt.Fprintln(out, "Users already exist; not creating an admin.")
	} else {
		admin, err := createAdmin(ctx, p, authSvc, o.admin)
		if err != nil {
			return err
		}
		if !o.noConnect {
			offerAccounts(ctx, p, cfg, st, admin.ID)
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintf(out, "  bosund --config %s\n", g.configPath)
	fmt.Fprintln(out, "  bosun login")
	fmt.Fprintln(out, "  bosun chat \"hello\"")
	fmt.Fprintln(out, "Accounts (any time, for each family member after `bosun login`):")
	fmt.Fprintln(out, "  bosun connect email      mail (and iCloud/Fastmail calendar) with an app password")
	fmt.Fprintln(out, "  bosun connect calendar   a read-only calendar from its private iCal (ICS) link")
	fmt.Fprintln(out, "  Advanced: `bosun connect google --scopes caldav` for Google Calendar write access (needs a Google Cloud OAuth client).")
	return nil
}

// answer returns the flag value if it was set explicitly, otherwise prompts
// with the flag value as the default.
func answer(p *prompter, label, val string, set bool) (string, error) {
	if set {
		return val, nil
	}
	return p.line(label, val)
}

func createAdmin(ctx context.Context, p *prompter, a *auth.Service, username string) (*auth.User, error) {
	var err error
	if username == "" {
		def := "admin"
		if u := os.Getenv("USER"); auth.ValidateUsername(u) == nil {
			def = u
		}
		if username, err = p.line("Admin username", def); err != nil {
			return nil, err
		}
	}
	if err := auth.ValidateUsername(username); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		pw, err := p.password(fmt.Sprintf("Password for %s (min %d chars)", username, auth.MinPasswordLen))
		if err != nil {
			return nil, err
		}
		if err := auth.ValidatePassword(pw); err != nil {
			fmt.Fprintln(p.out, err)
			continue
		}
		confirm, err := p.password("Confirm password")
		if err != nil {
			return nil, err
		}
		if pw != confirm {
			fmt.Fprintln(p.out, "Passwords do not match.")
			continue
		}
		u, err := a.CreateUser(ctx, username, pw, auth.RoleAdmin)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(p.out, "Created admin user %q\n", username)
		return u, nil
	}
	return nil, errors.New("too many attempts")
}
