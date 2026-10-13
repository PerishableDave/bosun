package events

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBusPerUser(t *testing.T) {
	b := NewBus()
	a1 := b.Subscribe(1, 4)
	a2 := b.Subscribe(1, 4)
	other := b.Subscribe(2, 4)
	b.Publish(1, Event{Type: TypeText, Data: TextData{Delta: "hi"}})
	for _, s := range []*Subscription{a1, a2} {
		select {
		case ev := <-s.C:
			if ev.Type != TypeText {
				t.Errorf("type = %s", ev.Type)
			}
		default:
			t.Error("subscriber did not receive event")
		}
	}
	select {
	case ev := <-other.C:
		t.Errorf("other user received %+v", ev)
	default:
	}
	a1.Close()
	a1.Close() // idempotent
	if b.Subscribers(1) != 1 {
		t.Errorf("Subscribers = %d", b.Subscribers(1))
	}
	if _, ok := <-a1.C; ok {
		t.Error("closed subscription channel still open")
	}
	b.Publish(1, Event{Type: TypeDone}) // must not panic on closed sub
	a2.Close()
	other.Close()
	if b.Subscribers(1) != 0 || b.Subscribers(2) != 0 {
		t.Error("subscribers leaked")
	}
}

func TestBusDropsWhenFull(t *testing.T) {
	b := NewBus()
	s := b.Subscribe(1, 1)
	defer s.Close()
	b.Publish(1, Event{Type: TypeDone})
	b.Publish(1, Event{Type: TypeDone}) // dropped, must not block
	if s.Dropped() != 1 {
		t.Errorf("Dropped = %d", s.Dropped())
	}
}

func TestSSEWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(Event{Type: TypeApprovalResolved, Data: ApprovalResolvedData{ApprovalID: "a1", Decision: "approve"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(Event{Type: TypeDone}); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(Event{Type: "bad\ntype"}); err == nil {
		t.Error("newline in type accepted")
	}
	w.Comment("ping")
	body := rec.Body.String()
	want := "event: approval_resolved\ndata: {\"approval_id\":\"a1\",\"decision\":\"approve\"}\n\nevent: done\ndata: {}\n\n: ping\n\n"
	if body != want {
		t.Errorf("body = %q\nwant  %q", body, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
}
