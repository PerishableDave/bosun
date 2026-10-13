package store

import (
	"context"
	"errors"
	"testing"
)

func TestChannelLinks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")

	l, err := s.CreateChannelLink(ctx, ChannelLink{Channel: "telegram", ExternalID: "42", UserID: alice.ID, DisplayName: "Alice"})
	if err != nil || l.ID == 0 || l.CreatedAt.IsZero() {
		t.Fatalf("create = %+v, %v", l, err)
	}
	if _, err := s.CreateChannelLink(ctx, ChannelLink{Channel: "telegram", ExternalID: "42", UserID: bob.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate link err = %v, want ErrConflict", err)
	}
	if _, err := s.CreateChannelLink(ctx, ChannelLink{Channel: "slack", ExternalID: "42", UserID: bob.ID}); err != nil {
		t.Fatalf("same id on another channel: %v", err)
	}
	got, err := s.GetChannelLink(ctx, "telegram", "42")
	if err != nil || got.UserID != alice.ID || got.DisplayName != "Alice" || got.ConversationID != "" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := s.GetChannelLink(ctx, "telegram", "43"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing err = %v", err)
	}

	conv, err := s.CreateConversation(ctx, alice.ID, "telegram", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetChannelLinkConversation(ctx, "telegram", "42", conv.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetChannelLink(ctx, "telegram", "42"); got.ConversationID != conv.ID {
		t.Fatalf("conversation = %q", got.ConversationID)
	}
	// Deleting the conversation clears the pointer, not the link.
	if err := s.DeleteConversation(ctx, alice.ID, conv.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetChannelLink(ctx, "telegram", "42"); err != nil || got.ConversationID != "" {
		t.Fatalf("after conversation delete = %+v, %v", got, err)
	}

	if ls, err := s.ListChannelLinks(ctx, alice.ID, ""); err != nil || len(ls) != 1 {
		t.Fatalf("list alice = %v, %v", ls, err)
	}
	if ls, err := s.ListChannelLinks(ctx, bob.ID, "telegram"); err != nil || len(ls) != 0 {
		t.Fatalf("list bob telegram = %v, %v", ls, err)
	}
	if ls, err := s.ListChannelLinksByChannel(ctx, "telegram"); err != nil || len(ls) != 1 {
		t.Fatalf("by channel = %v, %v", ls, err)
	}
	// Another user cannot delete alice's link.
	if err := s.DeleteChannelLink(ctx, bob.ID, "telegram", "42"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete by other user err = %v", err)
	}
	if err := s.DeleteChannelLink(ctx, alice.ID, "telegram", "42"); err != nil {
		t.Fatal(err)
	}

	// Deleting a user cascades to their links.
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM users WHERE id = ?`, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetChannelLink(ctx, "slack", "42"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link survived user deletion: %v", err)
	}
}
