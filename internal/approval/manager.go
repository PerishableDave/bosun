package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

// ErrCanceled is returned by Wait when the approval was canceled (its waiting
// run went away) by another Wait whose ctx ended.
var ErrCanceled = errors.New("approval: canceled")

// DecisionExpired and DecisionCanceled are the approval_resolved event
// Decision values used for the non-user outcomes (alongside "approve" and
// "deny").
const (
	DecisionExpired  = "expired"
	DecisionCanceled = "canceled"
)

// DefaultTimeout is used when Options.Timeout is zero (matches the
// config default for policy.approval_timeout).
const DefaultTimeout = 15 * time.Minute

// Store is the subset of *store.Store the manager needs.
type Store interface {
	CreateApproval(ctx context.Context, a store.Approval) (*store.Approval, error)
	GetApproval(ctx context.Context, id string) (*store.Approval, error)
	ListPendingApprovals(ctx context.Context, userID int64) ([]*store.Approval, error)
	ResolveApproval(ctx context.Context, id, status string, resolvedBy int64) (*store.Approval, error)
	ExpireApprovals(ctx context.Context) ([]*store.Approval, error)
}

var _ Store = (*store.Store)(nil)

// Options configure a StoreManager.
type Options struct {
	// Timeout is the default approval lifetime (policy.approval_timeout).
	Timeout time.Duration
	// Audit, if set, receives a KindApproval entry when an approval is
	// requested and when it is resolved, expired, or canceled.
	Audit audit.Logger
	// Log receives operational errors. Nil discards.
	Log *slog.Logger
	// PollInterval bounds how long Wait can miss a resolution made by
	// another process sharing the database (in-process resolutions wake
	// waiters immediately). Default 2s.
	PollInterval time.Duration
	// OnResolve, if set, is called after an approval leaves the pending
	// state: resolved by the user (Approve / Deny), expired
	// (Decision(DecisionExpired)), or canceled (Decision(DecisionCanceled)).
	// It runs synchronously on the resolving goroutine after the state change
	// is persisted and published, so it must not block; panics are
	// recovered. The daemon wires it to the on_approval hooks.
	OnResolve func(ctx context.Context, a Approval, d Decision)
	// PreviewDir is where preview images are kept while approvals are
	// pending ($data_dir/approvals). Empty disables previews.
	PreviewDir string
	// AllowScope, if set, creates the user's "allow" rule for (capability,
	// scope); ResolveAlways calls it before approving. The daemon wires it
	// to the policy engine.
	AllowScope func(ctx context.Context, userID int64, c integration.Capability, scope string) error
}

// StoreManager is the database-backed Manager. Approvals are persisted, so
// they survive restarts (pending ones past their deadline are expired by
// ExpireStale, which the daemon calls at startup). Waiters in this process
// are woken immediately by Resolve; expiry is enforced by a timer in Wait and
// by the periodic sweep in Run.
type StoreManager struct {
	st      Store
	bus     events.Publisher
	timeout time.Duration
	aud     audit.Logger
	log     *slog.Logger
	poll    time.Duration
	onRes   func(ctx context.Context, a Approval, d Decision)
	preview string
	allow   func(ctx context.Context, userID int64, c integration.Capability, scope string) error

	mu      sync.Mutex
	waiters map[string]chan struct{} // closed (and removed) when the approval changes state
}

var _ Manager = (*StoreManager)(nil)

// NewManager returns a StoreManager. bus may be nil (no events).
func NewManager(st Store, bus events.Publisher, opts Options) *StoreManager {
	m := &StoreManager{st: st, bus: bus, timeout: opts.Timeout, aud: opts.Audit, log: opts.Log, poll: opts.PollInterval,
		onRes: opts.OnResolve, waiters: map[string]chan struct{}{}, preview: opts.PreviewDir, allow: opts.AllowScope}
	if m.timeout <= 0 {
		m.timeout = DefaultTimeout
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.poll <= 0 {
		m.poll = 2 * time.Second
	}
	return m
}

func fromStore(a *store.Approval) *Approval {
	return &Approval{
		ID: a.ID, UserID: a.UserID, ConversationID: a.ConversationID, ToolCallID: a.ToolCallID, Tool: a.Tool,
		Capability: integration.Capability(a.Capability), Risk: integration.Risk(a.Risk), Scope: a.Scope, Args: a.Args,
		Status: Status(a.Status), CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt, ResolvedAt: a.ResolvedAt,
		ResolvedBy: a.ResolvedBy,
	}
}

// Request implements Manager.
func (m *StoreManager) Request(ctx context.Context, p Params) (*Approval, error) {
	if p.UserID <= 0 {
		return nil, fmt.Errorf("approval: request: invalid user id %d", p.UserID)
	}
	if p.Tool == "" || !p.Risk.Valid() {
		return nil, fmt.Errorf("approval: request: tool and a valid risk are required")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = m.timeout
	}
	a, err := m.st.CreateApproval(ctx, store.Approval{
		UserID: p.UserID, ConversationID: p.ConversationID, ToolCallID: p.ToolCallID, Tool: p.Tool,
		Capability: string(p.Capability), Risk: string(p.Risk), Scope: p.Scope, Args: p.Args, ExpiresAt: time.Now().Add(timeout),
	})
	if err != nil {
		return nil, fmt.Errorf("approval: request: %w", err)
	}
	out := fromStore(a)
	note := "approval requested"
	if len(p.Preview) > 0 {
		if err := m.writePreview(out.ID, p.Preview, p.PreviewMIME); err != nil {
			m.log.Warn("approval: preview not stored", "approval_id", out.ID, "err", err)
			note += "; preview not stored: " + err.Error()
		} else {
			out.HasPreview = true
			note += " with preview"
		}
	}
	m.record(ctx, out, "ask", 0, note)
	m.publish(out.UserID, events.Event{Type: events.TypeApprovalRequired, Data: events.ApprovalRequiredData{
		ApprovalID: out.ID, Tool: out.Tool, Args: out.Args, Risk: string(out.Risk), ConversationID: out.ConversationID,
		Scope: out.Scope, HasPreview: out.HasPreview,
	}})
	return out, nil
}

// --- previews ---

var previewExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg"}

// approvalIDRe matches the IDs store.NewID("appr_") generates; anything else
// is never turned into a file name.
var approvalIDRe = regexp.MustCompile(`^appr_[0-9a-f]{8,64}$`)

func (m *StoreManager) writePreview(id string, img []byte, mime string) error {
	if m.preview == "" {
		return errors.New("previews are not enabled")
	}
	ext, ok := previewExt[mime]
	if !ok {
		return fmt.Errorf("unsupported preview type %q", mime)
	}
	if len(img) > MaxPreviewBytes {
		return fmt.Errorf("preview is %d bytes (limit %d)", len(img), MaxPreviewBytes)
	}
	if !approvalIDRe.MatchString(id) {
		return fmt.Errorf("unexpected approval id %q", id)
	}
	if err := os.MkdirAll(m.preview, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.preview, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(img); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(m.preview, id+ext))
}

// previewPath returns the stored preview of id and its MIME type.
func (m *StoreManager) previewPath(id string) (string, string, bool) {
	if m.preview == "" || !approvalIDRe.MatchString(id) {
		return "", "", false
	}
	for mime, ext := range previewExt {
		p := filepath.Join(m.preview, id+ext)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, mime, true
		}
	}
	return "", "", false
}

func (m *StoreManager) removePreview(id string) {
	if m.preview == "" || !approvalIDRe.MatchString(id) {
		return
	}
	for _, ext := range previewExt {
		if err := os.Remove(filepath.Join(m.preview, id+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("approval: remove preview", "approval_id", id, "err", err)
		}
	}
}

// PreviewFile returns the path and MIME type of a pending approval's
// preview image. Only the owner may read it (ErrNotFound otherwise, to
// avoid leaking existence); ErrNoPreview if there is none (or the approval
// is no longer pending: previews are deleted on resolution).
func (m *StoreManager) PreviewFile(ctx context.Context, id string, userID int64) (string, string, error) {
	a, err := m.Get(ctx, id, userID)
	if err != nil {
		return "", "", err
	}
	if a.Status != StatusPending {
		return "", "", ErrNoPreview
	}
	p, mime, ok := m.previewPath(a.ID)
	if !ok {
		return "", "", ErrNoPreview
	}
	return p, mime, nil
}

// SweepPreviews deletes preview files whose approval is no longer pending
// (or no longer exists), e.g. left behind by a crash. The daemon runs it at
// startup and periodically. It returns how many files it removed.
func (m *StoreManager) SweepPreviews(ctx context.Context) (int, error) {
	if m.preview == "" {
		return 0, nil
	}
	ents, err := os.ReadDir(m.preview)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("approval: sweep previews: %w", err)
	}
	n := 0
	for _, e := range ents {
		name := e.Name()
		p := filepath.Join(m.preview, name)
		if strings.HasPrefix(name, ".tmp-") {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Hour {
				if os.Remove(p) == nil {
					n++
				}
			}
			continue
		}
		id := strings.TrimSuffix(name, filepath.Ext(name))
		if !approvalIDRe.MatchString(id) || previewExt[mimeOfExt(filepath.Ext(name))] == "" {
			continue // not ours; leave it
		}
		a, err := m.st.GetApproval(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return n, fmt.Errorf("approval: sweep previews: %w", err)
		}
		if err == nil && a.Status == store.ApprovalPending && time.Now().Before(a.ExpiresAt) {
			continue
		}
		if err := os.Remove(p); err == nil {
			n++
		}
	}
	return n, nil
}

func mimeOfExt(ext string) string {
	for mime, e := range previewExt {
		if e == ext {
			return mime
		}
	}
	return ""
}

// ResolveAlways creates userID's "allow" rule for the approval's
// (capability, scope) and then approves it: "approve and always allow on
// <scope>". It requires a scoped approval (ErrNoScope) that is still
// pending and owned by userID (same errors as Resolve). The rule is created
// only after those checks, so a stale or foreign approval never creates
// one.
func (m *StoreManager) ResolveAlways(ctx context.Context, id string, userID int64) error {
	cur, err := m.st.GetApproval(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("approval: resolve: %w", err)
	}
	if cur.UserID != userID || userID <= 0 {
		return ErrForbidden
	}
	switch {
	case cur.Status == store.ApprovalExpired, cur.Status == store.ApprovalPending && !time.Now().Before(cur.ExpiresAt):
		return ErrExpired
	case cur.Status != store.ApprovalPending:
		return ErrAlreadyResolved
	}
	if cur.Scope == "" {
		return ErrNoScope
	}
	if m.allow == nil {
		return errors.New("approval: always-allow is not available")
	}
	if err := m.allow(ctx, userID, integration.Capability(cur.Capability), cur.Scope); err != nil {
		return fmt.Errorf("approval: always allow: %w", err)
	}
	return m.Resolve(ctx, id, userID, Approve)
}

// Get returns an approval by ID, enforcing ownership (ErrNotFound if userID
// does not own it, to avoid leaking existence).
func (m *StoreManager) Get(ctx context.Context, id string, userID int64) (*Approval, error) {
	a, err := m.st.GetApproval(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && a.UserID != userID) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("approval: get: %w", err)
	}
	return m.decorate(fromStore(a)), nil
}

// decorate fills HasPreview.
func (m *StoreManager) decorate(a *Approval) *Approval {
	if a.Status == StatusPending {
		_, _, a.HasPreview = m.previewPath(a.ID)
	}
	return a
}

// waiter returns the channel that will be closed on the next state change of id.
func (m *StoreManager) waiter(id string) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.waiters[id]
	if !ok {
		ch = make(chan struct{})
		m.waiters[id] = ch
	}
	return ch
}

func (m *StoreManager) wake(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch, ok := m.waiters[id]; ok {
		close(ch)
		delete(m.waiters, id)
	}
}

// Wait implements Manager. Outcomes:
//   - approved: (Approve, nil)
//   - denied: (Deny, nil)
//   - expired: (Deny, ErrExpired)
//   - canceled elsewhere: (Deny, ErrCanceled)
//   - ctx done: the approval is canceled and (Deny, ctx.Err()) is returned.
//
// Any store error returns Deny with the error (fail closed).
func (m *StoreManager) Wait(ctx context.Context, id string) (Decision, error) {
	// Drop the wake channel on exit so finished approvals do not accumulate;
	// any concurrent waiter just re-reads the (by then terminal) status.
	defer m.wake(id)
	for {
		// Register before reading so a Resolve between the read and the
		// select still wakes us.
		wake := m.waiter(id)
		a, err := m.st.GetApproval(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return Deny, m.cancel(id, ctx.Err())
			}
			if errors.Is(err, store.ErrNotFound) {
				return Deny, ErrNotFound
			}
			return Deny, fmt.Errorf("approval: wait: %w", err)
		}
		switch Status(a.Status) {
		case StatusApproved:
			return Approve, nil
		case StatusDenied:
			return Deny, nil
		case StatusExpired:
			return Deny, ErrExpired
		case StatusCanceled:
			return Deny, ErrCanceled
		case StatusPending:
		default:
			return Deny, fmt.Errorf("approval: wait: unknown status %q", a.Status)
		}
		d := time.Until(a.ExpiresAt)
		if d <= 0 {
			m.expireDue(ctx)
			// The store compares at millisecond precision; avoid a hot loop if
			// it has not yet considered the row due.
			d = time.Millisecond
		}
		d = min(d, m.poll)
		t := time.NewTimer(d)
		select {
		case <-wake:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return Deny, m.cancel(id, ctx.Err())
		}
		t.Stop()
	}
}

// cancel marks id canceled because its waiter went away. If the approval was
// resolved concurrently, the resolution stands; cause is returned either way.
func (m *StoreManager) cancel(id string, cause error) error {
	// ctx is done; use a short detached context for the bookkeeping write.
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	a, err := m.st.ResolveApproval(ctx, id, store.ApprovalCanceled, 0)
	if err == nil {
		m.removePreview(id)
		out := fromStore(a)
		m.record(ctx, out, DecisionCanceled, 0, "waiting run went away")
		m.publish(out.UserID, events.Event{Type: events.TypeApprovalResolved,
			Data: events.ApprovalResolvedData{ApprovalID: id, Decision: DecisionCanceled, ConversationID: out.ConversationID}})
		m.wake(id)
		m.notify(ctx, out, DecisionCanceled)
	} else if !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
		m.log.Error("approval: cancel failed", "approval_id", id, "err", err)
	}
	return cause
}

// Resolve implements Manager. Only the owning user may resolve; a resolution
// by anyone else returns ErrForbidden. The first answer wins atomically (the
// store's conditional update); later answers get ErrAlreadyResolved, and
// answers after the deadline get ErrExpired.
func (m *StoreManager) Resolve(ctx context.Context, id string, userID int64, d Decision) error {
	if !d.Valid() {
		return fmt.Errorf("approval: resolve: invalid decision %q", d)
	}
	cur, err := m.st.GetApproval(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("approval: resolve: %w", err)
	}
	if cur.UserID != userID || userID <= 0 {
		return ErrForbidden
	}
	status := store.ApprovalDenied
	if d == Approve {
		status = store.ApprovalApproved
	}
	a, err := m.st.ResolveApproval(ctx, id, status, userID)
	if errors.Is(err, store.ErrConflict) {
		after, gerr := m.st.GetApproval(ctx, id)
		if gerr != nil {
			return fmt.Errorf("approval: resolve: %w", gerr)
		}
		if after.Status == store.ApprovalExpired {
			return ErrExpired
		}
		if after.Status == store.ApprovalPending { // past deadline but not yet swept
			m.expireDue(ctx)
			return ErrExpired
		}
		return ErrAlreadyResolved
	}
	if err != nil {
		return fmt.Errorf("approval: resolve: %w", err)
	}
	m.removePreview(id)
	out := fromStore(a)
	m.record(ctx, out, string(d), userID, "")
	m.publish(out.UserID, events.Event{Type: events.TypeApprovalResolved,
		Data: events.ApprovalResolvedData{ApprovalID: id, Decision: string(d), ConversationID: out.ConversationID}})
	m.wake(id)
	m.notify(ctx, out, d)
	return nil
}

// ListPending implements Manager.
func (m *StoreManager) ListPending(ctx context.Context, userID int64) ([]*Approval, error) {
	as, err := m.st.ListPendingApprovals(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("approval: list pending: %w", err)
	}
	out := make([]*Approval, 0, len(as))
	for _, a := range as {
		out = append(out, m.decorate(fromStore(a)))
	}
	return out, nil
}

// ExpireStale marks every pending approval past its deadline as expired,
// notifying owners and waking waiters. Call it at daemon startup (approvals
// whose waiting run died with the previous process) and periodically (Run).
func (m *StoreManager) ExpireStale(ctx context.Context) (int, error) {
	as, err := m.st.ExpireApprovals(ctx)
	if err != nil {
		return 0, fmt.Errorf("approval: expire: %w", err)
	}
	for _, a := range as {
		m.removePreview(a.ID)
		out := fromStore(a)
		m.record(ctx, out, DecisionExpired, 0, "approval timed out")
		m.publish(out.UserID, events.Event{Type: events.TypeApprovalResolved,
			Data: events.ApprovalResolvedData{ApprovalID: out.ID, Decision: DecisionExpired, ConversationID: out.ConversationID}})
		m.wake(out.ID)
		m.notify(ctx, out, DecisionExpired)
	}
	return len(as), nil
}

func (m *StoreManager) expireDue(ctx context.Context) {
	if _, err := m.ExpireStale(ctx); err != nil {
		m.log.Error("approval: expire sweep failed", "err", err)
	}
}

// Run sweeps expired approvals every interval (default 30s) until ctx ends.
func (m *StoreManager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.expireDue(ctx)
		}
	}
}

// notify calls Options.OnResolve, recovering panics.
func (m *StoreManager) notify(ctx context.Context, a *Approval, d Decision) {
	if m.onRes == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			m.log.Error("approval: OnResolve panicked", "approval_id", a.ID, "panic", fmt.Sprint(p))
		}
	}()
	m.onRes(ctx, *a, d)
}

func (m *StoreManager) publish(userID int64, ev events.Event) {
	if m.bus != nil {
		m.bus.Publish(userID, ev)
	}
}

func (m *StoreManager) record(ctx context.Context, a *Approval, decision string, approver int64, note string) {
	if m.aud == nil {
		return
	}
	e := audit.Entry{
		UserID: a.UserID, Kind: audit.KindApproval, Tool: a.Tool, Capability: a.Capability, Risk: a.Risk,
		Decision: decision, ApprovalID: a.ID, ApproverID: approver, Args: a.Args,
	}
	if note != "" {
		e.Notes = []string{note}
	}
	if err := m.aud.Record(ctx, e); err != nil {
		m.log.Error("approval: audit failed", "approval_id", a.ID, "err", err)
	}
}
