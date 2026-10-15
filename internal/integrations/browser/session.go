package browser

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec" // allowed for this integration only; see integrations_test.go
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

//go:embed page.js
var pageJS string

// startTimeout bounds launching Chromium and opening its first tab.
const startTimeout = 45 * time.Second

// session is one user's Chromium: its own process, profile directory and
// guard proxy, with a single tab the agent drives. Page actions are
// serialized by mu.
type session struct {
	userID int64

	mu sync.Mutex // serializes page actions

	tabCtx      context.Context
	tabCancel   context.CancelFunc
	allocCancel context.CancelFunc
	proxy       *guardProxy

	cmdMu sync.Mutex
	cmd   *exec.Cmd

	st           sync.Mutex // guards the fields below
	url, title   string
	lastUsed     time.Time
	pendingUntil time.Time // an approval for this session's page is pending
	latest       []byte    // latest screenshot (PNG)
	latestAt     time.Time
	dialogs      []string // dialogs dismissed since the last read
	preview      *previewMark
	closed       bool
}

// previewMark remembers the element shown in an approval preview, so the
// approved call can check the page did not change underneath it.
type previewMark struct {
	ref  string
	fp   string
	url  string
	when time.Time
}

// launch starts Chromium for userID.
func (in *Integration) launch(userID int64) (*session, error) {
	s := &session{userID: userID, lastUsed: time.Now()}
	profile := in.profileDir(userID)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, fmt.Errorf("browser profile: %w", err)
	}
	// A stale lock from a crashed run would make Chromium refuse the
	// profile; only this process ever uses it.
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(profile, f))
	}
	px, err := newGuardProxy(in.set.AllowPrivate)
	if err != nil {
		return nil, err
	}
	s.proxy = px
	env := []string{"HOME=" + profile, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8"}
	if t := os.Getenv("TMPDIR"); t != "" {
		env = append(env, "TMPDIR="+t)
	}
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(in.chromePath),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(profile),
		chromedp.NoInheritEnv,
		chromedp.Env(env...),
		chromedp.ProxyServer(px.URL()),
		// Loopback is otherwise bypassed implicitly; send it through the
		// guard too.
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		chromedp.WindowSize(viewportW, viewportH),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-features", "Translate,MediaRouter,OptimizationHints,AutofillServerCommunication,DialMediaRouteProvider,InterestFeedContentSuggestions"),
		chromedp.Flag("no-pings", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.ModifyCmdFunc(func(cmd *exec.Cmd) {
			setProcAttrs(cmd)
			s.cmdMu.Lock()
			s.cmd = cmd
			s.cmdMu.Unlock()
		}),
	}
	if in.set.headless() {
		opts = append(opts, chromedp.Flag("headless", true))
	}
	if in.set.NoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)
	s.tabCtx, s.tabCancel, s.allocCancel = tabCtx, tabCancel, allocCancel

	started := make(chan error, 1)
	go func() {
		// The first call starts the browser; it must not carry a timeout.
		_, err := chromedp.CallBrowser(tabCtx, browser.SetDownloadBehavior,
			browser.SetDownloadBehaviorParams{Behavior: browser.SetDownloadBehaviorBehaviorDeny})
		if err == nil {
			err = chromedp.Do(tabCtx, chromedp.Navigate("about:blank"))
		}
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			s.close()
			return nil, fmt.Errorf("start Chromium (%s): %w", in.chromePath, err)
		}
	case <-time.After(startTimeout):
		s.close()
		return nil, fmt.Errorf("start Chromium (%s): timed out after %s", in.chromePath, startTimeout)
	}
	go s.handleDialogs()
	return s, nil
}

// handleDialogs dismisses JavaScript dialogs (alert, confirm, prompt) so a
// page cannot stall the session, and accepts "leave page" prompts. The
// message is reported with the next read.
func (s *session) handleDialogs() {
	for ev, err := range chromedp.Events(s.tabCtx, page.JavascriptDialogOpening) {
		if err != nil {
			return
		}
		s.st.Lock()
		if len(s.dialogs) < 5 {
			s.dialogs = append(s.dialogs, fmt.Sprintf("%s: %s", ev.Type, clip(ev.Message, 200)))
		}
		s.st.Unlock()
		accept := ev.Type == page.DialogTypeBeforeunload || ev.Type == page.DialogTypeAlert
		ctx, cancel := context.WithTimeout(s.tabCtx, 5*time.Second)
		_, _ = chromedp.Call(ctx, page.HandleJavaScriptDialog, page.HandleJavaScriptDialogParams{Accept: accept})
		cancel()
	}
}

// alive reports whether the browser is still usable.
func (s *session) alive() bool {
	s.st.Lock()
	closed := s.closed
	s.st.Unlock()
	return !closed && s.tabCtx.Err() == nil
}

// close shuts Chromium down (gracefully, then by killing its process
// group) and stops the proxy. Safe to call more than once.
func (s *session) close() {
	s.st.Lock()
	if s.closed {
		s.st.Unlock()
		return
	}
	s.closed = true
	s.st.Unlock()
	if s.tabCtx != nil && s.tabCtx.Err() == nil {
		ctx, cancel := context.WithTimeout(s.tabCtx, 5*time.Second)
		_ = chromedp.Cancel(ctx)
		cancel()
	}
	if s.tabCancel != nil {
		s.tabCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	s.cmdMu.Lock()
	if s.cmd != nil && s.cmd.Process != nil {
		killGroup(s.cmd.Process.Pid)
	}
	s.cmdMu.Unlock()
	if s.proxy != nil {
		_ = s.proxy.Close()
	}
}

// actx derives an action context from the tab, bounded by d and ended
// when parent ends.
func (s *session) actx(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(s.tabCtx, d)
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// js runs one operation of page.js in a fresh isolated world of the main
// frame and decodes its result into out.
func (s *session) js(ctx context.Context, op string, args any, out any) error {
	tree, err := chromedp.Call(ctx, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return fmt.Errorf("page: %w", err)
	}
	if tree.FrameTree == nil || tree.FrameTree.Frame == nil {
		return errors.New("page: no main frame")
	}
	w, err := chromedp.Call(ctx, page.CreateIsolatedWorld, page.CreateIsolatedWorldParams{
		FrameID: tree.FrameTree.Frame.ID, WorldName: "bosun"})
	if err != nil {
		return fmt.Errorf("page: %w", err)
	}
	if args == nil {
		args = struct{}{}
	}
	a, err := json.Marshal(args)
	if err != nil {
		return err
	}
	expr := "(" + pageJS + ")(" + strconv.Quote(op) + "," + string(a) + ")"
	raw, err := chromedp.Run(ctx, chromedp.Evaluate[[]byte](expr, func(p *runtime.EvaluateParams) {
		p.ContextID = w.ExecutionContextID
	}))
	if err != nil {
		return fmt.Errorf("page script: %w", err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("page script result: %w", err)
	}
	return nil
}

type pageState struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Ready string `json:"ready"`
}

// settle waits briefly for a navigation or re-render an action may have
// started: a short grace period, then until the document is complete (at
// most max).
func (s *session) settle(ctx context.Context, max time.Duration) pageState {
	deadline := time.Now().Add(max)
	var st pageState
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	if !sleep(400 * time.Millisecond) {
		return st
	}
	for time.Now().Before(deadline) {
		var cur pageState
		if err := s.js(ctx, "state", nil, &cur); err == nil {
			st = cur
			if cur.Ready == "complete" {
				break
			}
		}
		if !sleep(200 * time.Millisecond) {
			break
		}
	}
	s.st.Lock()
	if st.URL != "" {
		s.url, s.title = st.URL, st.Title
	}
	s.st.Unlock()
	return st
}

// screenshot captures the viewport as PNG and keeps it as the latest view.
func (s *session) screenshot(ctx context.Context) ([]byte, error) {
	b, err := chromedp.Run(ctx, chromedp.CaptureScreenshot())
	if err != nil {
		return nil, fmt.Errorf("screenshot: %w", err)
	}
	s.st.Lock()
	s.latest, s.latestAt = b, time.Now()
	s.st.Unlock()
	return b, nil
}

func (s *session) touch() {
	s.st.Lock()
	s.lastUsed = time.Now()
	s.st.Unlock()
}

func (s *session) current() (url, title string) {
	s.st.Lock()
	defer s.st.Unlock()
	return s.url, s.title
}

func (s *session) takeDialogs() []string {
	s.st.Lock()
	defer s.st.Unlock()
	d := s.dialogs
	s.dialogs = nil
	return d
}
