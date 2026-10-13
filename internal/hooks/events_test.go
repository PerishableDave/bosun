package hooks

import (
	"context"
	"encoding/json"
	"testing"
)

func TestEmitEvent(t *testing.T) {
	r := NewRegistry()
	var got []string
	if err := r.OnEvent("a", func(_ context.Context, ev Event) { got = append(got, "a:"+ev.Name+":"+string(ev.Payload)) }); err != nil {
		t.Fatal(err)
	}
	if err := r.OnEvent("b", func(_ context.Context, ev Event) { got = append(got, "b:"+ev.Name) }); err != nil {
		t.Fatal(err)
	}
	if err := r.OnEvent("a", func(context.Context, Event) {}); err == nil {
		t.Error("duplicate name accepted")
	}
	if err := r.OnEvent("c", nil); err == nil {
		t.Error("nil handler accepted")
	}
	if err := r.EmitEvent(context.Background(), Event{Name: "email.received", UserID: 3, Payload: json.RawMessage(`{"x":1}`)}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `a:email.received:{"x":1}` || got[1] != "b:email.received" {
		t.Errorf("handlers saw %q", got)
	}
	for _, ev := range []Event{
		{Name: ""}, {Name: "Email.Received"}, {Name: "email..x"}, {Name: "e", UserID: -1},
		{Name: "e", Payload: json.RawMessage(`{`)},
	} {
		if err := r.EmitEvent(context.Background(), ev); err == nil {
			t.Errorf("EmitEvent(%+v) accepted", ev)
		}
	}
	if len(got) != 2 {
		t.Errorf("invalid events reached handlers: %q", got)
	}
}

func TestValidEventName(t *testing.T) {
	for name, want := range map[string]bool{
		"email.received": true, "calendar.event_soon": true, "x": true, "a-b.c1": true,
		"": false, "1x": false, ".x": false, "x.": false, "x y": false, "X": false,
	} {
		if got := ValidEventName(name); got != want {
			t.Errorf("ValidEventName(%q) = %v", name, got)
		}
	}
}
