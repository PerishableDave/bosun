package providers

import (
	"reflect"
	"testing"

	"github.com/perishabledave/bosun/internal/config"
)

func TestFromConfig(t *testing.T) {
	t.Setenv("TEST_ANTH_KEY", "k")
	cfg := config.Default()
	cfg.LLM.Providers = map[string]config.ProviderConfig{
		"anthropic": {APIKeyEnv: "TEST_ANTH_KEY"},
		"ollama":    {BaseURL: "http://localhost:11434"},
		"openai":    {BaseURL: "http://localhost:1234/v1"},
	}
	reg, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Names(); !reflect.DeepEqual(got, []string{"anthropic", "ollama", "openai"}) {
		t.Fatalf("names = %v", got)
	}
	if _, _, err := reg.Resolve("ollama/llama3"); err != nil {
		t.Fatal(err)
	}
}

func TestFromConfig_Default(t *testing.T) {
	reg, err := FromConfig(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Names()) != 2 {
		t.Fatalf("names = %v", reg.Names())
	}
}

func TestFromConfig_Unknown(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.Providers = map[string]config.ProviderConfig{"bogus": {}}
	if _, err := FromConfig(cfg); err == nil {
		t.Fatal("want error")
	}
}
