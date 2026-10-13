package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/integrations/calendar"
	"github.com/dperedo/bosun/internal/integrations/email"
	"github.com/dperedo/bosun/internal/providers"
	"github.com/dperedo/bosun/internal/store"
	"github.com/dperedo/bosun/internal/vault"
)

// detectKeychain finds the OS keychain for the vault master key, exactly as
// bosund does. Tests replace it so they never touch the real keychain.
var detectKeychain = func() vault.Keychain { return vault.DetectKeychain(vault.ExecRunner{}) }

// localTester checks a credential in-process (setup runs before bosund).
// Tests replace it.
var localTester = func(ctx context.Context, integ string, cred integration.Credential) error {
	switch integ {
	case "email":
		return email.New().TestCredential(ctx, cred)
	case "calendar":
		return calendar.New().TestCredential(ctx, cred)
	}
	return fmt.Errorf("cannot test %s credentials", integ)
}

// localBackend tests credentials in-process and stores them straight into
// the vault, for `bosun setup` (no daemon or login yet).
type localBackend struct {
	v    *vault.Vault
	user int64
}

func (b localBackend) test(ctx context.Context, integ string, req api.TestCredentialRequest) error {
	err := localTester(ctx, integ, integration.Credential{Kind: req.Kind, Secret: req.Secret, Fields: req.Fields})
	if err != nil {
		return &testFailedError{msg: strings.ReplaceAll(err.Error(), req.Secret, "[redacted]")}
	}
	return nil
}

func (b localBackend) store(ctx context.Context, integ, kind string, req api.PutCredentialRequest) error {
	uid := b.user
	return b.v.Put(ctx, &uid, integ, kind, integration.Credential{Kind: kind, Secret: req.Secret, Fields: req.Fields})
}

// offerAccounts runs after the first admin is created: the email wizard
// (which continues with the calendar), or, if declined, the ICS calendar
// flow. Failures are reported but never fail setup; end of input means no.
func offerAccounts(ctx context.Context, p *prompter, cfg *config.Config, st *store.Store, userID int64) {
	out := p.out
	ask := func(q string, def bool) bool {
		ok, err := p.yesNo(q, def)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintln(out, err)
			}
			return false
		}
		return ok
	}
	fmt.Fprintln(out)
	wantEmail := ask("Connect an email account now? (uses an app password; about two minutes)", true)
	wantICS := false
	if !wantEmail {
		fmt.Fprintln(out, "Skipped. Later: `bosun connect email`.")
		wantICS = ask("Add a read-only calendar from its private iCal (ICS) link now?", false)
		if !wantICS {
			return
		}
	}
	keys, _, err := vault.SelectKeySource(ctx, vault.KeySourceOptions{
		Mode: cfg.Daemon.MasterKey, DataDir: cfg.Daemon.DataDir, Detect: detectKeychain,
	})
	if err != nil {
		fmt.Fprintf(out, "Cannot open the credential vault (%v); connect accounts later with `bosun connect email`.\n", err)
		return
	}
	w := &wizard{p: p, out: out, be: localBackend{v: vault.New(st, keys), user: userID}, interactive: true}
	if wantEmail {
		err = w.email(ctx, emailOpts{})
	} else {
		err = w.ics(ctx, providers.OtherProvider(), "", false, "")
	}
	if err != nil {
		fmt.Fprintf(out, "%v\nYou can try again later with `bosun connect %s`.\n", err, map[bool]string{true: "email", false: "calendar"}[wantEmail])
	}
}
