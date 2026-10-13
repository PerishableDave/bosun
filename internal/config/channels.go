package config

import (
	"fmt"
	"net/url"
	"regexp"
)

// ChannelsConfig configures chat-app channels ("text your assistant").
type ChannelsConfig struct {
	Telegram TelegramConfig `yaml:"telegram,omitempty"`
}

// TelegramConfig configures the Telegram bot channel. The bot token is the
// shared vault credential channels/telegram_bot_token (`bosun credentials
// set channels telegram_bot_token --shared`), or the environment variable
// named by TokenEnv.
type TelegramConfig struct {
	Enabled bool `yaml:"enabled"`
	// TokenEnv names an environment variable holding the bot token; when
	// set and non-empty it takes precedence over the vault.
	TokenEnv string `yaml:"token_env,omitempty"`
	// AllowedUsernames, if non-empty, limits which Telegram usernames may
	// link a chat (case-insensitive, "@" optional).
	AllowedUsernames []string `yaml:"allowed_usernames,omitempty"`
	// APIURL overrides the Bot API endpoint (default
	// https://api.telegram.org), e.g. for a self-hosted Bot API server.
	APIURL string `yaml:"api_url,omitempty"`
}

var (
	envNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	tgUsernameRe = regexp.MustCompile(`^@?[A-Za-z0-9_]{1,64}$`)
)

func (c *Config) validateChannels() []error {
	var errs []error
	tg := c.Channels.Telegram
	if tg.TokenEnv != "" && !envNameRe.MatchString(tg.TokenEnv) {
		errs = append(errs, fmt.Errorf("channels.telegram.token_env %q: not a valid environment variable name", tg.TokenEnv))
	}
	for _, u := range tg.AllowedUsernames {
		if !tgUsernameRe.MatchString(u) {
			errs = append(errs, fmt.Errorf("channels.telegram.allowed_usernames: %q is not a Telegram username", u))
		}
	}
	if tg.APIURL != "" {
		u, err := url.Parse(tg.APIURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("channels.telegram.api_url %q: must be an http(s) URL", tg.APIURL))
		}
	}
	return errs
}
