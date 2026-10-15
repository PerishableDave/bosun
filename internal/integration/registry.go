package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	// capability / tool names: dot-separated lowercase segments.
	dottedRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z0-9][a-z0-9_-]*)+$`)
)

// Registry holds registered integrations and indexes their tools. It is safe
// for concurrent use.
type Registry struct {
	mu           sync.RWMutex
	integrations map[string]Integration
	tools        map[string]registeredTool
}

type registeredTool struct {
	tool        Tool
	integration string
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{integrations: map[string]Integration{}, tools: map[string]registeredTool{}}
}

// Validate checks an integration's manifest and tools against the rules:
//   - manifest has a valid Name, a valid Scope, and at least one capability;
//   - every capability is "<Name>.<...>" and declared once;
//   - every tool has a valid unique "<Name>."-prefixed name, a non-nil Handler, a valid Risk,
//     valid JSON InputSchema (if set), and a Capability that appears in
//     Manifest.Capabilities.
func Validate(i Integration) error {
	if i == nil {
		return errors.New("integration: nil integration")
	}
	m := i.Manifest()
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("integration %q: "+format, append([]any{m.Name}, args...)...))
	}
	if !nameRe.MatchString(m.Name) {
		add("invalid name")
	}
	if !m.Scope.Valid() {
		add("manifest scope %q must be %q or %q", m.Scope, ScopePerUser, ScopeShared)
	}
	if len(m.Capabilities) == 0 {
		add("manifest declares no capabilities")
	}
	if len(m.ConfigSchema) > 0 && !json.Valid(m.ConfigSchema) {
		add("config schema is not valid JSON")
	}
	seenCap := map[Capability]bool{}
	for _, c := range m.Capabilities {
		if !dottedRe.MatchString(string(c)) {
			add("invalid capability %q", c)
		} else if !strings.HasPrefix(string(c), m.Name+".") {
			add("capability %q must be in the %q namespace", c, m.Name+".")
		}
		if seenCap[c] {
			add("capability %q declared twice", c)
		}
		seenCap[c] = true
	}
	seenTool := map[string]bool{}
	for _, t := range i.Tools() {
		if !dottedRe.MatchString(t.Name) {
			add("invalid tool name %q", t.Name)
		} else if !strings.HasPrefix(t.Name, m.Name+".") {
			add("tool %q must be in the %q namespace", t.Name, m.Name+".")
		}
		if seenTool[t.Name] {
			add("duplicate tool %q", t.Name)
		}
		seenTool[t.Name] = true
		if !seenCap[t.Capability] {
			add("tool %q uses capability %q not declared in manifest", t.Name, t.Capability)
		}
		if !t.Risk.Valid() {
			add("tool %q has invalid risk %q", t.Name, t.Risk)
		}
		if t.Handler == nil {
			add("tool %q has no handler", t.Name)
		}
		if len(t.InputSchema) > 0 && !json.Valid(t.InputSchema) {
			add("tool %q input schema is not valid JSON", t.Name)
		}
		if t.ScopeArg != "" && !schemaHasProperty(t.InputSchema, t.ScopeArg) {
			add("tool %q scope argument %q is not a property of its input schema", t.Name, t.ScopeArg)
		}
	}
	return errors.Join(errs...)
}

// Register validates i and adds it. It rejects integrations whose name, or any
// of whose tool names, is already registered. On error nothing is added.
func (r *Registry) Register(i Integration) error {
	if err := Validate(i); err != nil {
		return err
	}
	m := i.Manifest()
	tools := i.Tools()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.integrations[m.Name]; dup {
		return fmt.Errorf("integration %q already registered", m.Name)
	}
	for _, t := range tools {
		if prev, dup := r.tools[t.Name]; dup {
			return fmt.Errorf("integration %q: tool %q already registered by %q", m.Name, t.Name, prev.integration)
		}
	}
	r.integrations[m.Name] = i
	for _, t := range tools {
		r.tools[t.Name] = registeredTool{tool: t, integration: m.Name}
	}
	return nil
}

// Integration returns the integration registered under name.
func (r *Registry) Integration(name string) (Integration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	i, ok := r.integrations[name]
	return i, ok
}

// Tool returns a registered tool and the name of its integration.
func (r *Registry) Tool(name string) (Tool, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t.tool, t.integration, ok
}

// Tools returns all registered tools sorted by name.
func (r *Registry) Tools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns registered integration names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.integrations))
	for n := range r.integrations {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// schemaHasProperty reports whether a JSON Schema object declares a
// top-level property name.
func schemaHasProperty(schema json.RawMessage, name string) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &s) != nil {
		return false
	}
	_, ok := s.Properties[name]
	return ok
}
