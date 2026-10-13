// Package providers builds an llm.Registry from configuration.
package providers

import (
	"fmt"
	"os"
	"sort"

	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/llm"
	"github.com/dperedo/bosun/internal/llm/anthropic"
	"github.com/dperedo/bosun/internal/llm/ollama"
	"github.com/dperedo/bosun/internal/llm/openai"
)

// FromConfig registers every provider listed in cfg.LLM.Providers. Supported
// keys are "anthropic", "openai", and "ollama". An api_key_env that names an
// unset variable yields an empty key (the provider's request will then fail
// with the server's auth error); the key itself is never logged.
func FromConfig(cfg *config.Config) (*llm.Registry, error) {
	reg := llm.NewRegistry()
	names := make([]string, 0, len(cfg.LLM.Providers))
	for n := range cfg.LLM.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := cfg.LLM.Providers[name]
		key := ""
		if pc.APIKeyEnv != "" {
			key = os.Getenv(pc.APIKeyEnv)
		}
		var p llm.Provider
		switch name {
		case "anthropic":
			p = anthropic.New(anthropic.Config{APIKey: key, BaseURL: pc.BaseURL})
		case "openai":
			base := pc.BaseURL
			if base == "" {
				base = "https://api.openai.com/v1"
			}
			op, err := openai.New(openai.Config{Name: "openai", BaseURL: base, APIKey: key})
			if err != nil {
				return nil, fmt.Errorf("llm provider %q: %w", name, err)
			}
			p = op
		case "ollama":
			op, err := ollama.New(ollama.Config{BaseURL: pc.BaseURL})
			if err != nil {
				return nil, fmt.Errorf("llm provider %q: %w", name, err)
			}
			p = op
		default:
			return nil, fmt.Errorf("llm provider %q: unknown provider (supported: anthropic, openai, ollama)", name)
		}
		if err := reg.Register(p); err != nil {
			return nil, fmt.Errorf("llm provider %q: %w", name, err)
		}
	}
	return reg, nil
}
