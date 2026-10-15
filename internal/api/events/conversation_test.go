package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConversationEvents(t *testing.T) {
	up := Event{Type: TypeConversationUpdated, Data: ConversationUpdatedData{ID: "conv_1", ConversationID: "conv_1",
		Title: "Trip", TitleSource: "auto", Unread: true, UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}}
	if ConversationOf(up) != "conv_1" {
		t.Error("ConversationOf(conversation_updated)")
	}
	if ConversationOf(Event{Type: TypeConversationDeleted, Data: ConversationDeletedData{ID: "conv_2"}}) != "conv_2" {
		t.Error("ConversationOf(conversation_deleted)")
	}
	b, _ := json.Marshal(up.Data)
	want := `{"id":"conv_1","title":"Trip","title_source":"auto","pinned":false,"archived":false,"unread":true,"updated_at":"2026-10-01T00:00:00Z","conversation_id":"conv_1"}`
	if string(b) != want {
		t.Errorf("payload = %s", b)
	}
	b, _ = json.Marshal(DoneData{ConversationID: "c"})
	if strings.Contains(string(b), "stopped") {
		t.Errorf("done without stop = %s", b)
	}
	b, _ = json.Marshal(DoneData{ConversationID: "c", Stopped: true})
	if string(b) != `{"conversation_id":"c","stopped":true}` {
		t.Errorf("stopped done = %s", b)
	}
}
