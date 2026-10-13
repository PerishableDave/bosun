package config

import (
	"strings"
	"testing"
)

func TestChannelsConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
channels:
  telegram:
    enabled: true
    token_env: BOSUN_TELEGRAM_TOKEN
    allowed_usernames: ["@alice", bob_99]
`))
	if err != nil {
		t.Fatal(err)
	}
	tg := cfg.Channels.Telegram
	if !tg.Enabled || tg.TokenEnv != "BOSUN_TELEGRAM_TOKEN" || len(tg.AllowedUsernames) != 2 {
		t.Fatalf("telegram = %+v", tg)
	}
	_, err = Parse([]byte(`
channels:
  telegram: { enabled: true, token_env: "BAD-NAME", allowed_usernames: ["not a name"], api_url: "ftp://x" }
`))
	for _, want := range []string{"token_env", "allowed_usernames", "api_url"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want mention of %s", err, want)
		}
	}
	if _, err := Parse([]byte("channels: { slack: {} }")); err == nil {
		t.Error("unknown channel accepted")
	}
}
