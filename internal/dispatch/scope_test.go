package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/policy"
	"github.com/perishabledave/bosun/internal/store"
)

// siteIntegration has scoped tools, a RiskFunc, and implements Previewer and
// ApprovalDetailer.
type siteIntegration struct {
	mu         sync.Mutex
	calls      []string
	conv       string
	previewErr error
	previews   int
	riskPanic  bool
}

func (s *siteIntegration) Manifest() integration.Manifest {
	return integration.Manifest{Name: "site", Version: "1", Scope: integration.ScopePerUser,
		Capabilities: []integration.Capability{"site.act", "site.submit"}}
}
func (s *siteIntegration) Init(context.Context, integration.Config, integration.CredentialSource) error {
	return nil
}
func (s *siteIntegration) Close() error { return nil }

func (s *siteIntegration) handler(name string) integration.Handler {
	return func(ctx context.Context, in json.RawMessage) (integration.Result, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, name)
		s.conv = integration.ConversationIDFromContext(ctx)
		return integration.Result{Content: name + " ok", AuditDetail: json.RawMessage(`{"step":1}`)}, nil
	}
}

func (s *siteIntegration) Tools() []integration.Tool {
	schema := json.RawMessage(`{"type":"object","properties":{"site":{"type":"string"},"ref":{"type":"string"}},"additionalProperties":false}`)
	return []integration.Tool{
		{Name: "site.click", Capability: "site.act", Risk: integration.RiskWrite, InputSchema: schema, ScopeArg: "site",
			Handler: s.handler("click"),
			RiskFunc: func(ctx context.Context, args json.RawMessage) integration.Risk {
				if s.riskPanic {
					panic("risk boom")
				}
				if _, ok := integration.UserIDFromContext(ctx); !ok {
					panic("RiskFunc without user")
				}
				if strings.Contains(string(args), "bank.example") {
					return integration.RiskDestructive
				}
				return integration.RiskRead // lowering is ignored
			}},
		{Name: "site.submit", Capability: "site.submit", Risk: integration.RiskDestructive, InputSchema: schema, ScopeArg: "site",
			Handler: s.handler("submit")},
	}
}

func (s *siteIntegration) Preview(ctx context.Context, tool string, args json.RawMessage) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.previews++
	if s.previewErr != nil {
		return nil, "", s.previewErr
	}
	return []byte("\x89PNGpreview"), "image/png", nil
}

func (s *siteIntegration) ApprovalDetails(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"element":"Reserve"}`), nil
}

type siteFx struct {
	d     *Dispatcher
	st    *store.Store
	eng   *policy.StoreEngine
	appr  *approval.StoreManager
	bus   *events.Bus
	integ *siteIntegration
	user  int64
	dir   string
}

func setupSite(t *testing.T) *siteFx {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateUser(ctx, "dave", "h", "member")
	integ := &siteIntegration{}
	reg := integration.NewRegistry()
	if err := reg.Register(integ); err != nil {
		t.Fatal(err)
	}
	eng := policy.NewEngine(st, policy.Options{Defaults: policy.Defaults{Read: policy.Allow, Write: policy.Ask, Destructive: policy.Ask}})
	bus := events.NewBus()
	dir := filepath.Join(t.TempDir(), "approvals")
	appr := approval.NewManager(st, bus, approval.Options{Timeout: time.Minute, PreviewDir: dir})
	d := New(reg, eng, appr, audit.NewStoreLogger(st, audit.Options{LogContent: true}), nil, bus)
	return &siteFx{d: d, st: st, eng: eng, appr: appr, bus: bus, integ: integ, user: u.ID, dir: dir}
}

// answer resolves the next approval_required event with dec and returns it.
func (f *siteFx) answer(t *testing.T, sub *events.Subscription, dec approval.Decision) chan events.ApprovalRequiredData {
	out := make(chan events.ApprovalRequiredData, 1)
	go func() {
		for ev := range sub.C {
			if d, ok := ev.Data.(events.ApprovalRequiredData); ok {
				out <- d
				_ = f.appr.Resolve(context.Background(), d.ApprovalID, f.user, dec)
				return
			}
		}
	}()
	return out
}

func (f *siteFx) dispatch(name, args string) (integration.Result, error) {
	return f.d.Dispatch(context.Background(), Call{UserID: f.user, ConversationID: "", ToolCallID: "tc", Name: name, Args: json.RawMessage(args)})
}

func TestScopedRuleAllowsWithoutAsking(t *testing.T) {
	f := setupSite(t)
	ctx := context.Background()
	if _, err := f.eng.SetScopedRule(ctx, f.user, "site.act", "opentable.com", policy.Allow); err != nil {
		t.Fatal(err)
	}
	// "WWW.OpenTable.com" normalizes to the rule's scope: no approval.
	res, err := f.dispatch("site.click", `{"site":"WWW.OpenTable.com","ref":"r1"}`)
	if err != nil || res.Content != "click ok" {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	es, _ := f.st.ListAudit(ctx, store.AuditFilter{Kind: string(audit.KindToolCall)})
	e := es[len(es)-1]
	if e.Decision != "allow" || string(e.Detail) != `{"step":1}` || !strings.Contains(strings.Join(e.Notes, ";"), "scope: opentable.com") {
		t.Errorf("audit = %+v", e)
	}
	// Another site falls back to the risk default (ask); deny it.
	sub := f.bus.Subscribe(f.user, 0)
	defer sub.Close()
	got := f.answer(t, sub, approval.Deny)
	if _, err := f.dispatch("site.click", `{"site":"example.com","ref":"r1"}`); !errors.Is(err, ErrDenied) {
		t.Fatalf("other site: %v", err)
	}
	if d := <-got; d.Scope != "example.com" || !d.HasPreview || !strings.Contains(string(d.Args), `"details":{"element":"Reserve"}`) {
		t.Errorf("approval event = %+v (args %s)", d, d.Args)
	}
	// The preview was deleted with the resolution.
	if ents, _ := os.ReadDir(f.dir); len(ents) != 0 {
		t.Errorf("previews left: %v", ents)
	}
}

func TestRiskFuncEscalatesNeverLowers(t *testing.T) {
	f := setupSite(t)
	ctx := context.Background()
	// A capability-wide allow and a scoped allow both exist ...
	f.eng.SetRule(ctx, f.user, "site.act", policy.Allow)
	f.eng.SetScopedRule(ctx, f.user, "site.act", "bank.example", policy.Allow)
	// ... a non-escalated call runs (RiskFunc returned Read: ignored, still Write; allowed by rule).
	if _, err := f.dispatch("site.click", `{"site":"shop.example","ref":"r1"}`); err != nil {
		t.Fatal(err)
	}
	// ... but the escalated call asks anyway.
	sub := f.bus.Subscribe(f.user, 0)
	defer sub.Close()
	got := f.answer(t, sub, approval.Approve)
	if _, err := f.dispatch("site.click", `{"site":"bank.example","ref":"r1"}`); err != nil {
		t.Fatalf("approved escalated call: %v", err)
	}
	if d := <-got; d.Risk != "destructive" {
		t.Errorf("approval risk = %s", d.Risk)
	}
	es, _ := f.st.ListAudit(ctx, store.AuditFilter{Kind: string(audit.KindToolCall)})
	e := es[len(es)-1]
	if e.Risk != "destructive" || !strings.Contains(strings.Join(e.Notes, ";"), "risk escalated from write to destructive") {
		t.Errorf("audit = %+v", e)
	}
	// An explicit deny still denies without asking.
	f.eng.SetScopedRule(ctx, f.user, "site.act", "bank.example", policy.Deny)
	if _, err := f.dispatch("site.click", `{"site":"bank.example","ref":"r1"}`); !errors.Is(err, ErrDenied) {
		t.Errorf("deny: %v", err)
	}
	// A panicking RiskFunc fails closed (destructive → ask; deny the ask).
	f.integ.riskPanic = true
	got = f.answer(t, sub, approval.Deny)
	if _, err := f.dispatch("site.click", `{"site":"shop.example","ref":"r1"}`); !errors.Is(err, ErrDenied) {
		t.Errorf("panic: %v", err)
	}
	if d := <-got; d.Risk != "destructive" {
		t.Errorf("panic risk = %s", d.Risk)
	}
}

func TestPreviewErrorAbortsBeforeApproval(t *testing.T) {
	f := setupSite(t)
	f.integ.previewErr = errors.New("ref_9 not found on the page")
	_, err := f.dispatch("site.submit", `{"site":"example.com","ref":"ref_9"}`)
	if err == nil || !strings.Contains(err.Error(), "ref_9 not found") {
		t.Fatalf("err = %v", err)
	}
	if as, _ := f.appr.ListPending(context.Background(), f.user); len(as) != 0 {
		t.Errorf("approval created: %+v", as)
	}
	if len(f.integ.calls) != 0 {
		t.Errorf("handler ran")
	}
}

func TestScopeOf(t *testing.T) {
	for args, want := range map[string]string{
		`{"site":"https://www.Example.com/x"}`: "example.com",
		`{"site":42}`:                          "",
		`{}`:                                   "",
		`not json`:                             "",
	} {
		if got := ScopeOf(json.RawMessage(args), "site"); got != want {
			t.Errorf("ScopeOf(%s) = %q, want %q", args, got, want)
		}
	}
}
