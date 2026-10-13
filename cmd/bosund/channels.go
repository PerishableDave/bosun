package main

import (
	"context"
	"errors"
	"os"

	"github.com/dperedo/bosun/internal/approval"
	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/channels"
	"github.com/dperedo/bosun/internal/channels/telegram"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/vault"
)

// setupChannels builds the chat-app channels enabled under `channels:`.
// The manager always exists (GET /v1/channels lists nothing when no
// channel is enabled); channels are started by Serve once the API is up.
func (d *daemon) setupChannels(vlt *vault.Vault, appr approval.Manager, aud audit.Logger, dp deps) error {
	linker := channels.NewLinker(d.store, channels.LinkerOptions{Audit: aud, Log: d.log})
	d.channels = channels.NewManager(linker, d.log)
	tg := d.cfg.Channels.Telegram
	if !tg.Enabled {
		return nil
	}
	bot, err := telegram.New(telegram.Options{
		Token:            telegramToken(tg.TokenEnv, vlt),
		Linker:           linker,
		Bus:              d.bus,
		Runner:           d.agent,
		Store:            d.store,
		Approvals:        appr,
		Log:              d.log,
		APIURL:           tg.APIURL,
		AllowedUsernames: tg.AllowedUsernames,
		TokenRetry:       dp.ChannelTokenRetry,
	})
	if err != nil {
		return err
	}
	if err := d.channels.Add(bot); err != nil {
		return err
	}
	d.log.Info("telegram channel enabled", "allowlist", len(tg.AllowedUsernames) > 0, "token_env", tg.TokenEnv)
	return nil
}

// telegramToken reads the bot token from $token_env if set, else from the
// shared vault credential channels/telegram_bot_token.
func telegramToken(env string, vlt *vault.Vault) telegram.TokenFunc {
	return func(ctx context.Context) (string, error) {
		if env != "" {
			if v := os.Getenv(env); v != "" {
				return v, nil
			}
		}
		c, err := vlt.Get(ctx, nil, channels.CredentialIntegration, telegram.CredentialKind)
		if errors.Is(err, vault.ErrNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return c.Secret, nil
	}
}

// channelIntegrations returns the tool-less "channels" integration (so its
// credential kinds can be set and its health shows in GET
// /v1/integrations) when at least one channel is enabled.
func (d *daemon) channelIntegrations() []integration.Integration {
	if d.channels == nil || len(d.channels.Names()) == 0 {
		return nil
	}
	return []integration.Integration{d.channels.Integration()}
}

// startChannels starts the enabled channels; they stop when ctx ends.
func (d *daemon) startChannels(ctx context.Context) {
	if d.channels != nil {
		d.channels.Start(ctx)
	}
}

// closeChannels stops the channels (and their in-flight turns).
func (d *daemon) closeChannels() {
	if d.channels == nil {
		return
	}
	if err := d.channels.Close(); err != nil {
		d.log.Warn("channels close", "err", err)
	}
}
