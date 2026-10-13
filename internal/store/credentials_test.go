package store

import (
	"context"
	"errors"
	"testing"
)

func TestCredentials(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "alice")
	uid := u.ID
	row := Credential{UserID: &uid, Integration: "email", Kind: "apikey", Nonce: []byte{1}, Ciphertext: []byte{2}}
	c1, err := s.UpsertCredential(ctx, row)
	if err != nil || c1.UserID == nil || *c1.UserID != uid {
		t.Fatalf("upsert: %+v %v", c1, err)
	}
	row.Ciphertext = []byte{3}
	c2, err := s.UpsertCredential(ctx, row)
	if err != nil || c2.ID != c1.ID || c2.Ciphertext[0] != 3 {
		t.Fatalf("replace: %+v %v", c2, err)
	}
	// Shared rows (NULL user) are unique too.
	shared := Credential{Integration: "email", Kind: "apikey", Nonce: []byte{1}, Ciphertext: []byte{9}}
	s1, err := s.UpsertCredential(ctx, shared)
	if err != nil || s1.UserID != nil {
		t.Fatalf("shared: %+v %v", s1, err)
	}
	shared.Ciphertext = []byte{8}
	s2, err := s.UpsertCredential(ctx, shared)
	if err != nil || s2.ID != s1.ID {
		t.Fatalf("shared replace: %+v %v", s2, err)
	}
	if l, _ := s.ListCredentials(ctx, nil, ""); len(l) != 1 {
		t.Errorf("shared list = %d", len(l))
	}
	if l, _ := s.ListCredentials(ctx, &uid, "email"); len(l) != 1 {
		t.Errorf("user list = %d", len(l))
	}
	if l, _ := s.ListCredentials(ctx, &uid, "calendar"); len(l) != 0 {
		t.Errorf("filtered list = %d", len(l))
	}
	if err := s.DeleteCredential(ctx, &uid, "email", "apikey"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCredential(ctx, &uid, "email", "apikey"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if _, err := s.GetCredential(ctx, nil, "email", "apikey"); err != nil {
		t.Errorf("shared row deleted too: %v", err)
	}
	bad := int64(0)
	if _, err := s.UpsertCredential(ctx, Credential{UserID: &bad, Integration: "x", Kind: "y", Nonce: []byte{1}, Ciphertext: []byte{1}}); err == nil {
		t.Error("user 0 accepted")
	}
}
