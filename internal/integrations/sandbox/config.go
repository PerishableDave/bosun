package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Languages a run may use.
const (
	LangPython = "python"
	LangNode   = "node"
	LangShell  = "shell"
)

// languages lists the supported languages in display order.
var languages = []string{LangPython, LangNode, LangShell}

// DefaultImages are the pinned, published images used when the admin does
// not override them. Each must provide `sh` and `tar` (busybox is enough).
var DefaultImages = map[string]string{
	LangPython: "python:3.12-alpine",
	LangNode:   "node:22-alpine",
	LangShell:  "alpine:3.22",
}

// Defaults and hard limits.
const (
	DefaultTimeout       = 60 * time.Second
	MaxTimeout           = 300 * time.Second
	DefaultMemory        = "512m"
	DefaultWorkSize      = "64m"
	DefaultMaxConcurrent = 2
	maxMaxConcurrent     = 16

	PullAuto  = "auto"
	PullNever = "never"

	BackendDocker = "docker"
	BackendNone   = "none"
)

// Settings is the "sandbox" section of bosun.yaml.
type Settings struct {
	// Backend runs the code: "docker" (default) or "none" (disables the
	// integration's tool).
	Backend string `json:"backend,omitempty"`
	// Docker is the docker CLI to run (default: "docker" on PATH).
	Docker string `json:"docker,omitempty"`
	// Images overrides the image per language (python, node, shell).
	Images map[string]string `json:"images,omitempty"`
	// Timeout is the default and maximum wall-clock time of one run
	// (default 60s, at most 300s). A duration string ("90s") or seconds.
	Timeout Duration `json:"timeout,omitempty"`
	// Memory is the container memory limit in docker syntax (default 512m).
	// Swap is disabled (memory-swap = memory).
	Memory string `json:"memory,omitempty"`
	// WorkSize is the size of the /work tmpfs (default 64m). It counts
	// toward Memory.
	WorkSize string `json:"work_size,omitempty"`
	// Pull is "auto" (pull a missing image on first use, default) or
	// "never" (a missing image is an error; pre-pull it yourself).
	Pull string `json:"pull,omitempty"`
	// MaxConcurrent bounds simultaneous runs (default 2); further calls
	// fail fast with a busy error.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
}

// Duration decodes from a Go duration string ("60s", "2m") or a number of
// seconds.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("invalid duration %q", s)
		}
		*d = Duration(v)
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("invalid duration %s", b)
	}
	*d = Duration(f * float64(time.Second))
	return nil
}

var (
	// sizeRe matches docker's byte-size syntax as accepted for --memory.
	sizeRe = regexp.MustCompile(`^[1-9][0-9]*[bkmgBKMG]?$`)
	// imageRe is a conservative image reference: it cannot start with "-"
	// (which docker would parse as a flag) and has no spaces.
	imageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$`)
)

// parseSettings decodes and validates raw settings, filling defaults.
func parseSettings(raw json.RawMessage) (Settings, error) {
	var s Settings
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return s, fmt.Errorf("sandbox: decode config: %w", err)
		}
	}
	if s.Backend == "" {
		s.Backend = BackendDocker
	}
	if s.Backend != BackendDocker && s.Backend != BackendNone {
		return s, fmt.Errorf("sandbox: unknown backend %q (supported: docker, none)", s.Backend)
	}
	if s.Docker == "" {
		s.Docker = "docker"
	}
	images := map[string]string{}
	for k, v := range DefaultImages {
		images[k] = v
	}
	for k, v := range s.Images {
		if _, ok := DefaultImages[k]; !ok {
			return s, fmt.Errorf("sandbox: images: unknown language %q (supported: %s)", k, strings.Join(languages, ", "))
		}
		v = strings.TrimSpace(v)
		if !imageRe.MatchString(v) {
			return s, fmt.Errorf("sandbox: images.%s: invalid image reference %q", k, v)
		}
		images[k] = v
	}
	s.Images = images
	if s.Timeout == 0 {
		s.Timeout = Duration(DefaultTimeout)
	}
	if t := time.Duration(s.Timeout); t < time.Second || t > MaxTimeout {
		return s, fmt.Errorf("sandbox: timeout must be between 1s and %s", MaxTimeout)
	}
	if s.Memory == "" {
		s.Memory = DefaultMemory
	}
	if !sizeRe.MatchString(s.Memory) {
		return s, fmt.Errorf("sandbox: memory %q must be a size like 512m or 1g", s.Memory)
	}
	if s.WorkSize == "" {
		s.WorkSize = DefaultWorkSize
	}
	if !sizeRe.MatchString(s.WorkSize) {
		return s, fmt.Errorf("sandbox: work_size %q must be a size like 64m", s.WorkSize)
	}
	if s.Pull == "" {
		s.Pull = PullAuto
	}
	if s.Pull != PullAuto && s.Pull != PullNever {
		return s, fmt.Errorf("sandbox: pull must be %q or %q", PullAuto, PullNever)
	}
	if s.MaxConcurrent == 0 {
		s.MaxConcurrent = DefaultMaxConcurrent
	}
	if s.MaxConcurrent < 1 || s.MaxConcurrent > maxMaxConcurrent {
		return s, fmt.Errorf("sandbox: max_concurrent must be between 1 and %d", maxMaxConcurrent)
	}
	return s, nil
}
