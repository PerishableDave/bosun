package telegram

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

// A scoped approval with a preview: the photo is sent before the keyboard,
// which offers "Always on <site>"; pressing it creates the rule and approves.
func TestApprovalPreviewAndAlways(t *testing.T) {
	var mu sync.Mutex
	var rules []string
	e := newEnv(t, func(o *Options) {
		// Replace the manager with one that keeps previews and can create
		// scoped rules; newEnv's e.appr is swapped below to match.
		m := approval.NewManager(o.Store.(*store.Store), o.Bus, approval.Options{Timeout: time.Minute, PreviewDir: filepath.Join(t.TempDir(), "approvals"),
			AllowScope: func(_ context.Context, uid int64, c integration.Capability, scope string) error {
				mu.Lock()
				defer mu.Unlock()
				rules = append(rules, string(c)+"@"+scope)
				return nil
			}})
		o.Approvals = m
	})
	e.appr = e.bot.o.Approvals.(*approval.StoreManager)
	e.link(740, "alice_tg", e.alice)

	png := []byte("\x89PNG\r\n\x1a\npreview-bytes")
	a, err := e.appr.Request(e.ctx, approval.Params{UserID: e.alice.ID, Tool: "browser.click", Capability: "browser.act",
		Risk: integration.RiskWrite, Scope: "opentable.com", Args: json.RawMessage(`{"site":"opentable.com","ref":"ref_3"}`),
		Preview: png, PreviewMIME: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	msg := e.api.waitSent(t, 740, "Approval needed: browser.click")
	if !strings.Contains(msg.text(), "Site: opentable.com") {
		t.Errorf("text = %q", msg.text())
	}
	if d := msg.callbackData(); len(d) != 3 || d[1] != "a:"+a.ID+":a" {
		t.Fatalf("keyboard = %v", d)
	}
	photos := e.api.callsTo("sendPhoto", 740)
	if len(photos) != 1 || photos[0].Params["photo_bytes"] != float64(len(png)) || photos[0].ID > msg.ID {
		t.Fatalf("sendPhoto calls = %+v (keyboard message %d)", photos, msg.ID)
	}
	e.api.press(740, msg.ID, "a:"+a.ID+":a")
	waitFor(t, "always answer", func() bool {
		as := e.api.callsTo("answerCallbackQuery", 0)
		return len(as) == 1 && strings.Contains(as[0].text(), "allowed on this site")
	})
	mu.Lock()
	got := strings.Join(rules, ",")
	mu.Unlock()
	if got != "browser.act@opentable.com" {
		t.Errorf("rules = %q", got)
	}
	if cur, _ := e.appr.Get(e.ctx, a.ID, e.alice.ID); cur.Status != approval.StatusApproved {
		t.Errorf("status = %s", cur.Status)
	}

	// Destructive approvals get no "always" button.
	e.appr.Request(e.ctx, approval.Params{UserID: e.alice.ID, Tool: "browser.submit", Capability: "browser.submit",
		Risk: integration.RiskDestructive, Scope: "opentable.com"})
	msg = e.api.waitSent(t, 740, "Approval needed: browser.submit")
	if d := msg.callbackData(); len(d) != 2 {
		t.Errorf("destructive keyboard = %v", d)
	}
}
