package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/integration"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\nfake")

func withPreviews(t *testing.T, f *fixture, allow func(context.Context, int64, integration.Capability, string) error) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "approvals")
	f.m = NewManager(f.st, f.bus, Options{Timeout: time.Minute, PreviewDir: dir, AllowScope: allow})
	return dir
}

func TestPreviewRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := setup(t, time.Minute)
	dir := withPreviews(t, f, nil)
	sub := f.bus.Subscribe(f.alice, 0)
	defer sub.Close()

	a, err := f.m.Request(ctx, Params{UserID: f.alice, Tool: "browser.submit", Capability: "browser.submit",
		Risk: integration.RiskDestructive, Scope: "opentable.com", Args: json.RawMessage(`{"ref":"ref_1"}`),
		Preview: pngMagic, PreviewMIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.HasPreview || a.Scope != "opentable.com" {
		t.Fatalf("approval = %+v", a)
	}
	ev := next(t, sub)
	if d, ok := ev.Data.(events.ApprovalRequiredData); !ok || !d.HasPreview || d.Scope != "opentable.com" {
		t.Fatalf("event = %+v", ev)
	}
	// Served to the owner only.
	p, mime, err := f.m.PreviewFile(ctx, a.ID, f.alice)
	if err != nil || mime != "image/png" || filepath.Dir(p) != dir {
		t.Fatalf("PreviewFile = %q, %q, %v", p, mime, err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, pngMagic) {
		t.Errorf("file content = %q", b)
	}
	if _, _, err := f.m.PreviewFile(ctx, a.ID, f.bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user: err = %v", err)
	}
	list, _ := f.m.ListPending(ctx, f.alice)
	if len(list) != 1 || !list[0].HasPreview || list[0].Scope != "opentable.com" {
		t.Errorf("ListPending = %+v", list)
	}
	// Deleted on resolution.
	if err := f.m.Resolve(ctx, a.ID, f.alice, Deny); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("preview still on disk after resolve: %v", err)
	}
	if _, _, err := f.m.PreviewFile(ctx, a.ID, f.alice); !errors.Is(err, ErrNoPreview) {
		t.Errorf("after resolve: err = %v", err)
	}
}

func TestPreviewRejectedButApprovalCreated(t *testing.T) {
	ctx := context.Background()
	f := setup(t, time.Minute)
	withPreviews(t, f, nil)
	for _, p := range []Params{
		{Preview: pngMagic, PreviewMIME: "text/html"},
		{Preview: make([]byte, MaxPreviewBytes+1), PreviewMIME: "image/png"},
	} {
		p.UserID, p.Tool, p.Capability, p.Risk = f.alice, "browser.submit", "browser.submit", integration.RiskDestructive
		a, err := f.m.Request(ctx, p)
		if err != nil || a.HasPreview {
			t.Errorf("Request = %+v, %v", a, err)
		}
	}
}

func TestPreviewDeletedOnExpiryAndSwept(t *testing.T) {
	ctx := context.Background()
	f := setup(t, time.Minute)
	dir := withPreviews(t, f, nil)
	a, err := f.m.Request(ctx, Params{UserID: f.alice, Tool: "browser.submit", Capability: "browser.submit",
		Risk: integration.RiskDestructive, Preview: pngMagic, PreviewMIME: "image/png", Timeout: 20 * time.Millisecond})
	if err != nil || !a.HasPreview {
		t.Fatal(a, err)
	}
	time.Sleep(40 * time.Millisecond)
	if n, err := f.m.ExpireStale(ctx); n != 1 || err != nil {
		t.Fatalf("ExpireStale = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, a.ID+".png")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("preview survived expiry: %v", err)
	}
	// Leftovers (e.g. after a crash) are swept; pending ones and foreign
	// files are kept.
	b, _ := f.m.Request(ctx, Params{UserID: f.alice, Tool: "browser.submit", Capability: "browser.submit",
		Risk: integration.RiskDestructive, Preview: pngMagic, PreviewMIME: "image/png"})
	os.WriteFile(filepath.Join(dir, a.ID+".png"), pngMagic, 0o600)
	os.WriteFile(filepath.Join(dir, "appr_00000000000000000000000000000000.jpg"), pngMagic, 0o600)
	os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600)
	n, err := f.m.SweepPreviews(ctx)
	if n != 2 || err != nil {
		t.Errorf("SweepPreviews = %d, %v", n, err)
	}
	for name, want := range map[string]bool{b.ID + ".png": true, a.ID + ".png": false, "README": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
}

func TestResolveAlways(t *testing.T) {
	ctx := context.Background()
	f := setup(t, time.Minute)
	type rule struct {
		user  int64
		c     integration.Capability
		scope string
	}
	var rules []rule
	withPreviews(t, f, func(_ context.Context, u int64, c integration.Capability, s string) error {
		rules = append(rules, rule{u, c, s})
		return nil
	})
	scoped, _ := f.m.Request(ctx, Params{UserID: f.alice, Tool: "browser.click", Capability: "browser.act",
		Risk: integration.RiskWrite, Scope: "opentable.com"})
	unscoped := f.request(t, 0)

	if err := f.m.ResolveAlways(ctx, unscoped.ID, f.alice); !errors.Is(err, ErrNoScope) {
		t.Errorf("unscoped: %v", err)
	}
	if err := f.m.ResolveAlways(ctx, scoped.ID, f.bob); !errors.Is(err, ErrForbidden) {
		t.Errorf("other user: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("rules created before checks passed: %+v", rules)
	}
	if err := f.m.ResolveAlways(ctx, scoped.ID, f.alice); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0] != (rule{f.alice, "browser.act", "opentable.com"}) {
		t.Errorf("rules = %+v", rules)
	}
	got, _ := f.m.Get(ctx, scoped.ID, f.alice)
	if got.Status != StatusApproved {
		t.Errorf("status = %s", got.Status)
	}
	if err := f.m.ResolveAlways(ctx, scoped.ID, f.alice); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("second: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("rule created for a resolved approval")
	}
}
