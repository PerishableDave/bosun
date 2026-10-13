package approval

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dperedo/bosun/internal/api/events"
	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/integration"
	"github.com/dperedo/bosun/internal/store"
)

type fixture struct {
	st    *store.Store
	bus   *events.Bus
	m     *StoreManager
	alice int64
	bob   int64
}

func setup(t *testing.T, timeout time.Duration) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := st.CreateUser(ctx, "alice", "h", "member")
	b, _ := st.CreateUser(ctx, "bob", "h", "member")
	bus := events.NewBus()
	m := NewManager(st, bus, Options{Timeout: timeout, Audit: audit.NewStoreLogger(st, audit.Options{})})
	return &fixture{st: st, bus: bus, m: m, alice: a.ID, bob: b.ID}
}

func (f *fixture) request(t *testing.T, timeout time.Duration) *Approval {
	t.Helper()
	a, err := f.m.Request(context.Background(), Params{UserID: f.alice, Tool: "email.send", Capability: "email.send",
		Risk: integration.RiskWrite, Args: json.RawMessage(`{"to":"x@y.example"}`), Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func next(t *testing.T, sub *events.Subscription) events.Event {
	t.Helper()
	select {
	case ev := <-sub.C:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return events.Event{}
	}
}

func TestApprove(t *testing.T) {
	f := setup(t, time.Minute)
	sub := f.bus.Subscribe(f.alice, 0)
	defer sub.Close()
	other := f.bus.Subscribe(f.bob, 0)
	defer other.Close()

	a := f.request(t, 0)
	ev := next(t, sub)
	if ev.Type != events.TypeApprovalRequired || ev.Data.(events.ApprovalRequiredData).ApprovalID != a.ID {
		t.Fatalf("event = %+v", ev)
	}
	if time.Until(a.ExpiresAt) < 50*time.Second {
		t.Errorf("default timeout not applied: %v", a.ExpiresAt)
	}
	pend, err := f.m.ListPending(context.Background(), f.alice)
	if err != nil || len(pend) != 1 || pend[0].ID != a.ID {
		t.Fatalf("pending = %v %v", pend, err)
	}
	done := make(chan Decision)
	go func() {
		d, err := f.m.Wait(context.Background(), a.ID)
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		done <- d
	}()
	time.Sleep(20 * time.Millisecond)
	if err := f.m.Resolve(context.Background(), a.ID, f.alice, Approve); err != nil {
		t.Fatal(err)
	}
	if d := <-done; d != Approve {
		t.Errorf("decision = %s", d)
	}
	ev = next(t, sub)
	if r := ev.Data.(events.ApprovalResolvedData); ev.Type != events.TypeApprovalResolved || r.Decision != "approve" {
		t.Errorf("resolved event = %+v", ev)
	}
	select {
	case ev := <-other.C:
		t.Errorf("bob received alice's event: %+v", ev)
	default:
	}
	got, _ := f.m.Get(context.Background(), a.ID, f.alice)
	if got.Status != StatusApproved || got.ResolvedBy != f.alice {
		t.Errorf("stored = %+v", got)
	}
	if _, err := f.m.Get(context.Background(), a.ID, f.bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob Get = %v", err)
	}
	ents, _ := f.st.ListAudit(context.Background(), store.AuditFilter{Kind: string(audit.KindApproval)})
	if len(ents) != 2 || ents[0].Decision != "ask" || ents[1].Decision != "approve" || ents[1].ApproverID != f.alice {
		t.Errorf("audit = %+v", ents)
	}
}

func TestDeny(t *testing.T) {
	f := setup(t, time.Minute)
	a := f.request(t, 0)
	if err := f.m.Resolve(context.Background(), a.ID, f.alice, Deny); err != nil {
		t.Fatal(err)
	}
	d, err := f.m.Wait(context.Background(), a.ID)
	if d != Deny || err != nil {
		t.Errorf("Wait = %s, %v", d, err)
	}
}

func TestTimeout(t *testing.T) {
	f := setup(t, time.Minute)
	sub := f.bus.Subscribe(f.alice, 0)
	defer sub.Close()
	a := f.request(t, 80*time.Millisecond)
	next(t, sub)
	start := time.Now()
	d, err := f.m.Wait(context.Background(), a.ID)
	if d != Deny || !errors.Is(err, ErrExpired) {
		t.Fatalf("Wait = %s, %v", d, err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("took %v", el)
	}
	if ev := next(t, sub); ev.Data.(events.ApprovalResolvedData).Decision != DecisionExpired {
		t.Errorf("event = %+v", ev)
	}
	if err := f.m.Resolve(context.Background(), a.ID, f.alice, Approve); !errors.Is(err, ErrExpired) {
		t.Errorf("late Resolve = %v", err)
	}
}

func TestResolveAfterDeadlineBeforeSweep(t *testing.T) {
	f := setup(t, time.Minute)
	a := f.request(t, 30*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	if err := f.m.Resolve(context.Background(), a.ID, f.alice, Approve); !errors.Is(err, ErrExpired) {
		t.Errorf("Resolve = %v", err)
	}
	got, _ := f.m.Get(context.Background(), a.ID, f.alice)
	if got.Status != StatusExpired {
		t.Errorf("status = %s", got.Status)
	}
}

func TestWrongUser(t *testing.T) {
	f := setup(t, time.Minute)
	a := f.request(t, 0)
	if err := f.m.Resolve(context.Background(), a.ID, f.bob, Approve); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob Resolve = %v", err)
	}
	if err := f.m.Resolve(context.Background(), a.ID, 0, Approve); !errors.Is(err, ErrForbidden) {
		t.Errorf("user 0 Resolve = %v", err)
	}
	if err := f.m.Resolve(context.Background(), "appr_nope", f.alice, Approve); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
	if err := f.m.Resolve(context.Background(), a.ID, f.alice, "maybe"); err == nil {
		t.Error("invalid decision accepted")
	}
	got, _ := f.m.Get(context.Background(), a.ID, f.alice)
	if got.Status != StatusPending {
		t.Errorf("status changed: %s", got.Status)
	}
}

func TestDoubleResolve(t *testing.T) {
	f := setup(t, time.Minute)
	a := f.request(t, 0)
	if err := f.m.Resolve(context.Background(), a.ID, f.alice, Deny); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Resolve(context.Background(), a.ID, f.alice, Approve); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("second = %v", err)
	}
	if d, _ := f.m.Wait(context.Background(), a.ID); d != Deny {
		t.Errorf("first answer did not win: %s", d)
	}
}

func TestConcurrentWaitResolve(t *testing.T) {
	f := setup(t, time.Minute)
	const n = 8
	for range n {
		a := f.request(t, 0)
		var wg sync.WaitGroup
		results := make(chan Decision, 4)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d, err := f.m.Wait(context.Background(), a.ID)
				if err != nil {
					t.Errorf("Wait: %v", err)
				}
				results <- d
			}()
		}
		var wins, losses int
		var mu sync.Mutex
		var rw sync.WaitGroup
		for i := range 4 {
			rw.Add(1)
			go func() {
				defer rw.Done()
				d := Approve
				if i%2 == 1 {
					d = Deny
				}
				err := f.m.Resolve(context.Background(), a.ID, f.alice, d)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case errors.Is(err, ErrAlreadyResolved):
					losses++
				default:
					t.Errorf("Resolve: %v", err)
				}
			}()
		}
		rw.Wait()
		wg.Wait()
		close(results)
		if wins != 1 || losses != 3 {
			t.Fatalf("wins=%d losses=%d", wins, losses)
		}
		var first Decision
		for d := range results {
			if first == "" {
				first = d
			} else if d != first {
				t.Fatalf("waiters disagree: %s vs %s", first, d)
			}
		}
	}
}

func TestWaitCtxCancelCancelsApproval(t *testing.T) {
	f := setup(t, time.Minute)
	a := f.request(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	d, err := f.m.Wait(ctx, a.ID)
	if d != Deny || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait = %s, %v", d, err)
	}
	got, _ := f.m.Get(context.Background(), a.ID, f.alice)
	if got.Status != StatusCanceled {
		t.Errorf("status = %s", got.Status)
	}
	if err := f.m.Resolve(context.Background(), a.ID, f.alice, Approve); !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("Resolve after cancel = %v", err)
	}
}

func TestExpireStaleOnStartup(t *testing.T) {
	f := setup(t, time.Minute)
	stale := f.request(t, 10*time.Millisecond)
	fresh := f.request(t, time.Hour)
	time.Sleep(30 * time.Millisecond)
	// Simulate a restart: a fresh manager over the same DB.
	m2 := NewManager(f.st, f.bus, Options{})
	n, err := m2.ExpireStale(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("ExpireStale = %d, %v", n, err)
	}
	if got, _ := m2.Get(context.Background(), stale.ID, f.alice); got.Status != StatusExpired {
		t.Errorf("stale = %s", got.Status)
	}
	if got, _ := m2.Get(context.Background(), fresh.ID, f.alice); got.Status != StatusPending {
		t.Errorf("fresh = %s", got.Status)
	}
}

func TestWaitUnknown(t *testing.T) {
	f := setup(t, time.Minute)
	if d, err := f.m.Wait(context.Background(), "appr_missing"); d != Deny || !errors.Is(err, ErrNotFound) {
		t.Errorf("Wait = %s, %v", d, err)
	}
}

func TestRequestValidation(t *testing.T) {
	f := setup(t, time.Minute)
	if _, err := f.m.Request(context.Background(), Params{UserID: 0, Tool: "x.y", Risk: integration.RiskRead}); err == nil {
		t.Error("user 0 accepted")
	}
	if _, err := f.m.Request(context.Background(), Params{UserID: f.alice, Tool: "x.y", Risk: "bad"}); err == nil {
		t.Error("bad risk accepted")
	}
}

func TestOnResolve(t *testing.T) {
	f := setup(t, time.Minute)
	type call struct {
		id string
		d  Decision
	}
	var mu sync.Mutex
	var got []call
	f.m.onRes = func(_ context.Context, a Approval, d Decision) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, call{a.ID, d})
		if d == Deny {
			panic("hook bug") // must be recovered
		}
	}
	ctx := context.Background()

	approved := f.request(t, 0)
	if err := f.m.Resolve(ctx, approved.ID, f.alice, Approve); err != nil {
		t.Fatal(err)
	}
	denied := f.request(t, 0)
	if err := f.m.Resolve(ctx, denied.ID, f.alice, Deny); err != nil {
		t.Fatal(err)
	}
	// A rejected second answer does not notify.
	if err := f.m.Resolve(ctx, denied.ID, f.alice, Approve); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second resolve = %v", err)
	}
	expired := f.request(t, time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if _, err := f.m.ExpireStale(ctx); err != nil {
		t.Fatal(err)
	}
	canceled := f.request(t, 0)
	cctx, stop := context.WithCancel(ctx)
	stop()
	if _, err := f.m.Wait(cctx, canceled.ID); err == nil {
		t.Fatal("wait on canceled ctx succeeded")
	}

	want := []call{{approved.ID, Approve}, {denied.ID, Deny}, {expired.ID, DecisionExpired}, {canceled.ID, DecisionCanceled}}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("OnResolve calls = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
