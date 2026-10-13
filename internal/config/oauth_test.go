package config

import (
	"strings"
	"testing"
)

func TestOAuthConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
oauth:
  google: { client_id: "abc.apps.googleusercontent.com", client_secret_env: GOOGLE_OAUTH_CLIENT_SECRET }
  microsoft: { client_id: "00000000-0000-0000-0000-000000000000" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if g := cfg.OAuth["google"]; g.ClientID != "abc.apps.googleusercontent.com" || g.ClientSecretEnv != "GOOGLE_OAUTH_CLIENT_SECRET" {
		t.Errorf("google = %+v", g)
	}
	if cfg.OAuth["microsoft"].ClientID == "" {
		t.Error("microsoft client id missing")
	}
	_, err = Parse([]byte("oauth:\n  google: { client_secret_env: X }\n  Bad: { client_id: x }\n"))
	if err == nil || !strings.Contains(err.Error(), "oauth.google.client_id: required") || !strings.Contains(err.Error(), "oauth.Bad: invalid provider name") {
		t.Errorf("err = %v", err)
	}
	if _, err := Parse([]byte("oauth:\n  google: { client_id: x, client_secret: plaintext }\n")); err == nil {
		t.Error("plaintext client_secret key accepted")
	}
}
