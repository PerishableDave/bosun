package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type stub struct {
	m     Manifest
	tools []Tool
}

func (s *stub) Manifest() Manifest                                   { return s.m }
func (s *stub) Init(context.Context, Config, CredentialSource) error { return nil }
func (s *stub) Tools() []Tool                                        { return s.tools }
func (s *stub) Close() error                                         { return nil }

func okHandler(context.Context, json.RawMessage) (Result, error) { return Result{}, nil }

func emailStub() *stub {
	return &stub{
		m: Manifest{Name: "email", Version: "1", Scope: ScopePerUser,
			Capabilities: []Capability{"email.read", "email.send"}},
		tools: []Tool{
			{Name: "email.search", Capability: "email.read", Risk: RiskRead, Handler: okHandler},
			{Name: "email.send", Capability: "email.send", Risk: RiskWrite, Handler: okHandler,
				InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
}

func TestRegisterValid(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(emailStub()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tool, integ, ok := r.Tool("email.send")
	if !ok || integ != "email" || tool.Capability != "email.send" {
		t.Errorf("Tool = %+v, %q, %v", tool, integ, ok)
	}
	if got := r.Tools(); len(got) != 2 || got[0].Name != "email.search" {
		t.Errorf("Tools = %+v", got)
	}
	if _, ok := r.Integration("email"); !ok {
		t.Error("Integration not found")
	}
}

func TestRegisterRejectsUndeclaredCapability(t *testing.T) {
	s := emailStub()
	s.tools = append(s.tools, Tool{Name: "email.delete", Capability: "email.delete", Risk: RiskDestructive, Handler: okHandler})
	err := NewRegistry().Register(s)
	if err == nil || !strings.Contains(err.Error(), "not declared in manifest") {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(emailStub()); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(emailStub()); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("duplicate integration: %v", err)
	}
	// A failed Register must not partially register.
	other := &stub{
		m: Manifest{Name: "mail2", Scope: ScopePerUser, Capabilities: []Capability{"mail2.send"}},
		tools: []Tool{
			{Name: "mail2.send", Capability: "mail2.send", Risk: RiskWrite, Handler: okHandler},
			{Name: "email.send", Capability: "mail2.send", Risk: RiskWrite, Handler: okHandler},
		},
	}
	if err := r.Register(other); err == nil {
		t.Error("tool name collision accepted")
	}
	if _, _, ok := r.Tool("mail2.send"); ok {
		t.Error("partial registration after failure")
	}
	if _, ok := r.Integration("mail2"); ok {
		t.Error("integration registered after failure")
	}
}

func TestValidateRules(t *testing.T) {
	cases := map[string]func(s *stub){
		"missing scope":         func(s *stub) { s.m.Scope = "" },
		"bad scope":             func(s *stub) { s.m.Scope = "global" },
		"bad name":              func(s *stub) { s.m.Name = "Email!" },
		"no capabilities":       func(s *stub) { s.m.Capabilities = nil; s.tools = nil },
		"foreign namespace":     func(s *stub) { s.m.Capabilities = append(s.m.Capabilities, "calendar.write") },
		"duplicate capability":  func(s *stub) { s.m.Capabilities = append(s.m.Capabilities, "email.send") },
		"duplicate tool":        func(s *stub) { s.tools = append(s.tools, s.tools[0]) },
		"nil handler":           func(s *stub) { s.tools[0].Handler = nil },
		"bad risk":              func(s *stub) { s.tools[0].Risk = "spicy" },
		"bad schema":            func(s *stub) { s.tools[0].InputSchema = json.RawMessage(`{`) },
		"bad tool name":         func(s *stub) { s.tools[0].Name = "search" },
		"foreign tool name":     func(s *stub) { s.tools[0].Name = "calendar.search" },
		"empty tool capability": func(s *stub) { s.tools[0].Capability = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := emailStub()
			mutate(s)
			if err := NewRegistry().Register(s); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if err := NewRegistry().Register(nil); err == nil {
		t.Error("nil integration accepted")
	}
}

func TestSharedScopeAndMCPNamespace(t *testing.T) {
	s := &stub{
		m: Manifest{Name: "mcp", Scope: ScopeShared, Capabilities: []Capability{"mcp.github.create_issue"}},
		tools: []Tool{{Name: "mcp.github.create_issue", Capability: "mcp.github.create_issue",
			Risk: RiskWrite, Handler: okHandler}},
	}
	if err := NewRegistry().Register(s); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestContextUserAndConfigDecode(t *testing.T) {
	ctx := WithUserID(context.Background(), 42)
	if id, ok := UserIDFromContext(ctx); !ok || id != 42 {
		t.Errorf("UserIDFromContext = %d, %v", id, ok)
	}
	if _, ok := UserIDFromContext(context.Background()); ok {
		t.Error("empty ctx has user")
	}
	var v struct {
		Dir string `json:"dir"`
	}
	if err := (Config{Name: "notes", Settings: json.RawMessage(`{"dir":"/n"}`)}).Decode(&v); err != nil || v.Dir != "/n" {
		t.Errorf("Decode = %+v, %v", v, err)
	}
}
