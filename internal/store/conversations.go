package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Conversation is a conversations row.
type Conversation struct {
	ID        string
	UserID    int64
	Title     string
	RoutineID int64 // 0 if interactive
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Message is a messages row. Content is a JSON array of content blocks in the
// llm package's neutral format; the store does not interpret it.
type Message struct {
	ID             int64
	ConversationID string
	Role           string // "user" | "assistant" | "tool"
	Content        json.RawMessage
	CreatedAt      time.Time
}

const convCols = `id, user_id, title, routine_id, created_at, updated_at`

func scanConversation(r interface{ Scan(...any) error }) (*Conversation, error) {
	var c Conversation
	var rid sql.NullInt64
	var cr, up int64
	if err := r.Scan(&c.ID, &c.UserID, &c.Title, &rid, &cr, &up); err != nil {
		return nil, notFound(err)
	}
	c.RoutineID, c.CreatedAt, c.UpdatedAt = rid.Int64, fromMS(cr), fromMS(up)
	return &c, nil
}

// CreateConversation creates a conversation owned by userID. routineID may be 0.
func (s *Store) CreateConversation(ctx context.Context, userID int64, title string, routineID int64) (*Conversation, error) {
	now := s.now()
	c := &Conversation{ID: NewID("conv_"), UserID: userID, Title: title, RoutineID: routineID,
		CreatedAt: fromMS(ms(now)), UpdatedAt: fromMS(ms(now))}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations(id, user_id, title, routine_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, userID, title, nullInt(routineID), ms(now), ms(now))
	if err != nil {
		return nil, fmt.Errorf("create conversation: %w", err)
	}
	return c, nil
}

// GetConversation returns a conversation only if it belongs to userID;
// otherwise ErrNotFound (no existence leak across users).
func (s *Store) GetConversation(ctx context.Context, userID int64, id string) (*Conversation, error) {
	c, err := scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+convCols+` FROM conversations WHERE id = ? AND user_id = ?`, id, userID))
	if err != nil {
		return nil, fmt.Errorf("get conversation %s: %w", id, err)
	}
	return c, nil
}

// ListConversations returns a user's conversations, most recently updated
// first. limit <= 0 means no limit.
func (s *Store) ListConversations(ctx context.Context, userID int64, limit int) ([]*Conversation, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+convCols+` FROM conversations WHERE user_id = ? ORDER BY updated_at DESC, id LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	var out []*Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("list conversations: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetConversationTitle renames a conversation owned by userID.
func (s *Store) SetConversationTitle(ctx context.Context, userID int64, id, title string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE conversations SET title = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		title, ms(s.now()), id, userID)
	if err != nil {
		return fmt.Errorf("set conversation title: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set conversation title %s: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteConversation deletes a conversation (and its messages) owned by userID.
func (s *Store) DeleteConversation(ctx context.Context, userID int64, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete conversation %s: %w", id, ErrNotFound)
	}
	return nil
}

// AppendMessage appends a message to a conversation and bumps its updated_at.
// The caller must already have verified ownership of the conversation.
func (s *Store) AppendMessage(ctx context.Context, conversationID, role string, content json.RawMessage) (*Message, error) {
	if !json.Valid(content) {
		return nil, fmt.Errorf("append message: content is not valid JSON")
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("append message: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages(conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		conversationID, role, string(content), ms(now))
	if err != nil {
		return nil, fmt.Errorf("append message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("append message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, ms(now), conversationID); err != nil {
		return nil, fmt.Errorf("append message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("append message: %w", err)
	}
	return &Message{ID: id, ConversationID: conversationID, Role: role, Content: content, CreatedAt: fromMS(ms(now))}, nil
}

// ListMessages returns a conversation's messages in order.
func (s *Store) ListMessages(ctx context.Context, conversationID string) ([]*Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, role, content, created_at FROM messages WHERE conversation_id = ? ORDER BY id`,
		conversationID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		var m Message
		var content string
		var created int64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &content, &created); err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		m.Content, m.CreatedAt = json.RawMessage(content), fromMS(created)
		out = append(out, &m)
	}
	return out, rows.Err()
}
