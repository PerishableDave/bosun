// Package ollama provides an llm.Provider for a local Ollama server via its
// OpenAI-compatible /v1 endpoint.
package ollama

import (
	"errors"
	"net/http"
	"strings"

	"github.com/perishabledave/bosun/internal/llm/openai"
)

// DefaultBaseURL is Ollama's default listen address.
const DefaultBaseURL = "http://localhost:11434"

// Config configures the Ollama provider.
type Config struct {
	BaseURL    string       // server root, e.g. http://localhost:11434 (default); "/v1" is appended
	HTTPClient *http.Client // optional
}

// New returns an openai.Provider named "ollama" pointed at <BaseURL>/v1.
func New(cfg Config) (*openai.Provider, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimSuffix(base, "/v1")
	if base == "" {
		return nil, errors.New("ollama: empty base URL")
	}
	return openai.New(openai.Config{Name: "ollama", BaseURL: base + "/v1", HTTPClient: cfg.HTTPClient})
}
