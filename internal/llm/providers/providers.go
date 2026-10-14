// Package providers builds an llm.Registry from configuration.
package providers

import (
	"fmt"
	"sort"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/llm"
	"github.com/perishabledave/bosun/internal/llm/anthropic"
	"github.com/perishabledave/bosun/internal/llm/ollama"
	"github.com/perishabledave/bosun/internal/llm/openai"
)

// Option configures FromConfig.
type Option func(*options)

type options struct{ keys *Keys }

// WithKeys resolves API keys with k (vault, then environment) instead of
// the environment only.
func WithKeys(k *Keys) Option { return func(o *options) { o.keys = k } }

// FromConfig registers every provider listed in cfg.LLM.Providers. Supported
// keys are "anthropic", "openai", and "ollama". API keys are looked up per
// request (see Keys): the vault's shared llm/<provider> credential when
// WithKeys is given, else the api_key_env variable. A missing key sends no
// key (the provider's request then fails with the server's auth error); the
// key itself is never logged.
func FromConfig(cfg *config.Config, opts ...Option) (*llm.Registry, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.keys == nil {
		o.keys = NewKeys(cfg, nil)
	}
	reg := llm.NewRegistry()
	names := make([]string, 0, len(cfg.LLM.Providers))
	for n := range cfg.LLM.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := cfg.LLM.Providers[name]
		keyFunc := o.keys.KeyFunc(name)
		var p llm.Provider
		switch name {
		case "anthropic":
			p = anthropic.New(anthropic.Config{KeyFunc: keyFunc, BaseURL: pc.BaseURL})
		case "openai":
			base := pc.BaseURL
			if base == "" {
				base = "https://api.openai.com/v1"
			}
			op, err := openai.New(openai.Config{Name: "openai", BaseURL: base, KeyFunc: keyFunc})
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
