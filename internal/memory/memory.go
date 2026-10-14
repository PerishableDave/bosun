// Package memory is Bosun's per-user memory: facts, preferences, summaries,
// and notes the agent recalls into its system prompt, stored in the SQLite
// memories table (with an FTS5 index) and editable by the user.
//
// Every operation is scoped by user ID; there is no cross-user path.
package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/perishabledave/bosun/internal/store"
)

// Kind classifies a memory.
type Kind string

// Memory kinds.
const (
	KindFact       Kind = "fact"       // something about the user
	KindPreference Kind = "preference" // how the user likes things done
	KindSummary    Kind = "summary"    // summary of a past conversation (never created by extraction)
	KindNote       Kind = "note"       // something the user explicitly asked to remember
)

// Kinds lists every valid kind.
var Kinds = []Kind{KindFact, KindPreference, KindSummary, KindNote}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindFact, KindPreference, KindSummary, KindNote:
		return true
	}
	return false
}

// MaxContentLen bounds a memory's content in bytes.
const MaxContentLen = 4000

// Errors.
var (
	// ErrNotFound means no memory with that ID belongs to the user.
	ErrNotFound = errors.New("memory: not found")
	// ErrInvalid means a kind or content failed validation.
	ErrInvalid = errors.New("memory: invalid")
)

// Memory is one remembered item.
type Memory struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"-"`
	Kind      Kind      `json:"kind"`
	Content   string    `json:"content"`
	Source    string    `json:"source"` // e.g. "conv:<conversationID>", "user" for manual entries
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store reads and writes memories.
type Store struct {
	st *store.Store
}

// NewStore returns a Store backed by st (which must be migrated).
func NewStore(st *store.Store) *Store { return &Store{st: st} }

func fromRow(r *store.MemoryRow) Memory {
	return Memory{ID: r.ID, UserID: r.UserID, Kind: Kind(r.Kind), Content: r.Content, Source: r.Source,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func fromRows(rs []*store.MemoryRow) []Memory {
	out := make([]Memory, 0, len(rs))
	for _, r := range rs {
		out = append(out, fromRow(r))
	}
	return out
}

func mapErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// normalizeContent trims content and checks it is non-empty, valid UTF-8,
// and within MaxContentLen.
func normalizeContent(content string) (string, error) {
	c := strings.TrimSpace(content)
	switch {
	case c == "":
		return "", fmt.Errorf("%w: content is required", ErrInvalid)
	case !utf8.ValidString(c):
		return "", fmt.Errorf("%w: content is not valid UTF-8", ErrInvalid)
	case len(c) > MaxContentLen:
		return "", fmt.Errorf("%w: content is longer than %d bytes", ErrInvalid, MaxContentLen)
	}
	return c, nil
}

// Add stores a new memory for userID.
func (s *Store) Add(ctx context.Context, userID int64, kind Kind, content, source string) (Memory, error) {
	if !kind.Valid() {
		return Memory{}, fmt.Errorf("%w: unknown kind %q", ErrInvalid, kind)
	}
	c, err := normalizeContent(content)
	if err != nil {
		return Memory{}, err
	}
	r, err := s.st.CreateMemory(ctx, userID, string(kind), c, source)
	if err != nil {
		return Memory{}, fmt.Errorf("memory: add: %w", err)
	}
	return fromRow(r), nil
}

// Get returns the user's memory id, or ErrNotFound (also when it belongs to
// another user).
func (s *Store) Get(ctx context.Context, userID, id int64) (Memory, error) {
	r, err := s.st.GetMemory(ctx, userID, id)
	if err != nil {
		return Memory{}, fmt.Errorf("memory: get: %w", mapErr(err))
	}
	return fromRow(r), nil
}

// Update replaces the content of the user's memory id.
func (s *Store) Update(ctx context.Context, userID, id int64, content string) (Memory, error) {
	c, err := normalizeContent(content)
	if err != nil {
		return Memory{}, err
	}
	r, err := s.st.UpdateMemoryContent(ctx, userID, id, c)
	if err != nil {
		return Memory{}, fmt.Errorf("memory: update: %w", mapErr(err))
	}
	return fromRow(r), nil
}

// Delete removes the user's memory id.
func (s *Store) Delete(ctx context.Context, userID, id int64) error {
	if err := s.st.DeleteMemory(ctx, userID, id); err != nil {
		return fmt.Errorf("memory: delete: %w", mapErr(err))
	}
	return nil
}

// List returns the user's memories, most recently updated first. kind ""
// means every kind; limit <= 0 means no limit.
func (s *Store) List(ctx context.Context, userID int64, kind Kind, limit, offset int) ([]Memory, error) {
	if kind != "" && !kind.Valid() {
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalid, kind)
	}
	rs, err := s.st.ListMemories(ctx, userID, string(kind), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("memory: list: %w", err)
	}
	return fromRows(rs), nil
}

// Search returns the user's memories matching every word of query, best
// match first (FTS5 bm25).
func (s *Store) Search(ctx context.Context, userID int64, query string, limit int) ([]Memory, error) {
	return s.SearchKind(ctx, userID, query, "", limit, 0)
}

// SearchKind is Search restricted to one kind ("" = all) with an offset.
//
// Each word of query is quoted as an FTS5 string, so FTS operators and
// punctuation in the query are matched literally rather than parsed. If FTS5
// still rejects the expression, or no word survives sanitizing, the search
// falls back to a substring (LIKE) match on every word.
func (s *Store) SearchKind(ctx context.Context, userID int64, query string, kind Kind, limit, offset int) ([]Memory, error) {
	if kind != "" && !kind.Valid() {
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalid, kind)
	}
	words := strings.Fields(query)
	if len(words) == 0 {
		return []Memory{}, nil
	}
	return s.search(ctx, userID, kind, words, " AND ", limit, offset)
}

// search tries FTS5 with the quoted terms joined by op, then falls back to
// LIKE (all terms).
func (s *Store) search(ctx context.Context, userID int64, kind Kind, words []string, op string, limit, offset int) ([]Memory, error) {
	if match := ftsQuery(words, op); match != "" {
		rs, err := s.st.SearchMemoriesFTS(ctx, userID, string(kind), match, limit, offset)
		if err == nil {
			return fromRows(rs), nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("memory: search: %w", err)
		}
		// Not expressible in FTS5 syntax: fall through to LIKE.
	}
	rs, err := s.st.SearchMemoriesLike(ctx, userID, string(kind), words, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("memory: search: %w", err)
	}
	return fromRows(rs), nil
}

// ftsQuery quotes each word as an FTS5 string ("" escapes a quote) and joins
// them with op. Words with no letter or digit are dropped, since the FTS
// tokenizer would reduce them to nothing. It returns "" if no word is left.
func ftsQuery(words []string, op string) string {
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		if !strings.ContainsFunc(w, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, op)
}
