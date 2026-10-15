package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Conversation sources (conversations.source).
const (
	SourceChat     = "chat"     // created through the API (web UI, CLI)
	SourceRoutine  = "routine"  // a routine run
	SourceTelegram = "telegram" // a Telegram chat
)

// Title sources (conversations.title_source).
const (
	TitleAuto = "auto" // Bosun chose (or may still choose) the title
	TitleUser = "user" // the user chose it; never changed automatically
)

// Conversation is a conversations row plus a few derived fields.
type Conversation struct {
	ID          string
	UserID      int64
	Title       string
	TitleSource string // TitleAuto | TitleUser
	RoutineID   int64  // 0 if interactive
	Source      string // SourceChat | SourceRoutine | SourceTelegram
	Pinned      bool
	Archived    bool
	Unread      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// LastMessageAt is the time of the newest message (zero if none).
	LastMessageAt time.Time

	// Derived (read-only).

	// MessageCount counts the user and assistant messages (tool result
	// messages are not counted).
	MessageCount int
	// LastMessagePreview is the text of the newest user or assistant message
	// that has text, whitespace collapsed, at most PreviewLen characters.
	LastMessagePreview string
	// RoutineName is the name of the routine that created the conversation
	// ("" if none, or the routine was deleted).
	RoutineName string
}

// PreviewLen bounds Conversation.LastMessagePreview, in characters.
const PreviewLen = 120

// Message is a messages row. Content is a JSON array of content blocks in the
// llm package's neutral format; the store does not interpret it.
type Message struct {
	ID             int64
	ConversationID string
	Role           string // "user" | "assistant" | "tool"
	Content        json.RawMessage
	CreatedAt      time.Time
}

// textBlockCond keeps the text blocks of json_each(<message content>) b.
const textBlockCond = `json_extract(b.value, '$.type') = 'text'`

const convSelect = `SELECT c.id, c.user_id, c.title, c.title_source, c.routine_id, c.source,
	c.pinned, c.archived, c.unread, c.created_at, c.updated_at, c.last_message_at,
	(SELECT count(*) FROM messages m WHERE m.conversation_id = c.id AND m.role IN ('user', 'assistant')),
	COALESCE((SELECT r.name FROM routines r WHERE r.id = c.routine_id), ''),
	COALESCE((SELECT substr(group_concat(json_extract(b.value, '$.text'), ' '), 1, 1000)
		FROM json_each((SELECT m.content FROM messages m
			WHERE m.conversation_id = c.id AND m.role IN ('user', 'assistant')
			AND EXISTS (SELECT 1 FROM json_each(m.content) x WHERE json_extract(x.value, '$.type') = 'text'
				AND trim(json_extract(x.value, '$.text')) <> '')
			ORDER BY m.id DESC LIMIT 1)) b
		WHERE ` + textBlockCond + `), '')
	FROM conversations c`

func scanConversation(r interface{ Scan(...any) error }) (*Conversation, error) {
	var c Conversation
	var rid, last sql.NullInt64
	var cr, up int64
	var preview string
	if err := r.Scan(&c.ID, &c.UserID, &c.Title, &c.TitleSource, &rid, &c.Source,
		&c.Pinned, &c.Archived, &c.Unread, &cr, &up, &last,
		&c.MessageCount, &c.RoutineName, &preview); err != nil {
		return nil, notFound(err)
	}
	c.RoutineID, c.CreatedAt, c.UpdatedAt = rid.Int64, fromMS(cr), fromMS(up)
	if last.Valid {
		c.LastMessageAt = fromMS(last.Int64)
	}
	c.LastMessagePreview = Preview(preview, PreviewLen)
	return &c, nil
}

// Preview collapses whitespace in s and cuts it to at most n characters,
// ending with "…" when cut.
func Preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return strings.TrimRight(string(r[:n-1]), " ") + "…"
	}
	return s
}

// NewConversation describes a conversation to create.
type NewConversation struct {
	Title string
	// TitleSource defaults to TitleUser when Title is set, else TitleAuto.
	TitleSource string
	// Source defaults to SourceRoutine when RoutineID is set, else SourceChat.
	Source    string
	RoutineID int64
}

// CreateConversation creates a conversation owned by userID. routineID may be
// 0. The source is SourceRoutine for a routine's conversation, else
// SourceChat; a non-empty title counts as the user's (TitleUser).
func (s *Store) CreateConversation(ctx context.Context, userID int64, title string, routineID int64) (*Conversation, error) {
	return s.CreateConversationWith(ctx, userID, NewConversation{Title: title, RoutineID: routineID})
}

// CreateConversationWith creates a conversation owned by userID.
func (s *Store) CreateConversationWith(ctx context.Context, userID int64, nc NewConversation) (*Conversation, error) {
	if nc.Source == "" {
		nc.Source = SourceChat
		if nc.RoutineID != 0 {
			nc.Source = SourceRoutine
		}
	}
	if nc.TitleSource == "" {
		nc.TitleSource = TitleAuto
		if nc.Title != "" && nc.Source == SourceChat {
			nc.TitleSource = TitleUser
		}
	}
	id, now := NewID("conv_"), ms(s.now())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations(id, user_id, title, title_source, routine_id, source, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, nc.Title, nc.TitleSource, nullInt(nc.RoutineID), nc.Source, now, now)
	if err != nil {
		return nil, fmt.Errorf("create conversation: %w", err)
	}
	return s.GetConversation(ctx, userID, id)
}

// GetConversation returns a conversation only if it belongs to userID;
// otherwise ErrNotFound (no existence leak across users).
func (s *Store) GetConversation(ctx context.Context, userID int64, id string) (*Conversation, error) {
	c, err := scanConversation(s.db.QueryRowContext(ctx, convSelect+` WHERE c.id = ? AND c.user_id = ?`, id, userID))
	if err != nil {
		return nil, fmt.Errorf("get conversation %s: %w", id, err)
	}
	return c, nil
}

// ConversationFilter selects a page of ListConversationPage.
type ConversationFilter struct {
	// Archived: nil lists every conversation; otherwise only those whose
	// archived flag equals *Archived.
	Archived *bool
	// Query, if non-empty, keeps conversations whose title or the text of
	// a user or assistant message contains it (case-insensitive for ASCII).
	Query string
	// Limit bounds the page (<= 0: no limit).
	Limit int
	// Before is the cursor returned with the previous page ("" = first page).
	Before string
}

// ErrBadCursor is returned for a cursor ListConversationPage did not issue.
var ErrBadCursor = fmt.Errorf("store: invalid cursor")

// conversation list order: pinned first, then by activity (newest message,
// or creation for an empty conversation), newest first; id breaks ties.
const convOrder = ` ORDER BY c.pinned DESC, COALESCE(c.last_message_at, c.created_at) DESC, c.id DESC`

func encodeCursor(c *Conversation) string {
	act := ms(c.LastMessageAt)
	if c.LastMessageAt.IsZero() {
		act = ms(c.CreatedAt)
	}
	p := 0
	if c.Pinned {
		p = 1
	}
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d.%d.%s", p, act, c.ID)))
}

func decodeCursor(s string) (pinned int, act int64, id string, err error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, 0, "", ErrBadCursor
	}
	parts := strings.SplitN(string(b), ".", 3)
	if len(parts) != 3 || (parts[0] != "0" && parts[0] != "1") || parts[2] == "" {
		return 0, 0, "", ErrBadCursor
	}
	act, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, "", ErrBadCursor
	}
	pinned, _ = strconv.Atoi(parts[0])
	return pinned, act, parts[2], nil
}

// likePattern returns %q% with LIKE wildcards in q escaped by '\'.
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// ListConversationPage returns one page of a user's conversations (pinned
// first, then most recent activity first) and the cursor of the next page
// ("" when this is the last one).
func (s *Store) ListConversationPage(ctx context.Context, userID int64, f ConversationFilter) ([]*Conversation, string, error) {
	q := convSelect + ` WHERE c.user_id = ?`
	args := []any{userID}
	if f.Archived != nil {
		q += ` AND c.archived = ?`
		args = append(args, *f.Archived)
	}
	if f.Query != "" {
		pat := likePattern(f.Query)
		q += ` AND (c.title LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM messages m, json_each(m.content) b
			WHERE m.conversation_id = c.id AND m.role IN ('user', 'assistant') AND ` + textBlockCond + `
			AND json_extract(b.value, '$.text') LIKE ? ESCAPE '\'))`
		args = append(args, pat, pat)
	}
	if f.Before != "" {
		p, act, id, err := decodeCursor(f.Before)
		if err != nil {
			return nil, "", err
		}
		q += ` AND (c.pinned, COALESCE(c.last_message_at, c.created_at), c.id) < (?, ?, ?)`
		args = append(args, p, act, id)
	}
	limit := -1
	if f.Limit > 0 {
		limit = f.Limit + 1
	}
	q += convOrder + ` LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	var out []*Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, "", fmt.Errorf("list conversations: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list conversations: %w", err)
	}
	next := ""
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
		next = encodeCursor(out[len(out)-1])
	}
	return out, next, nil
}

// ListConversations returns all of a user's conversations (archived ones
// too) in list order. limit <= 0 means no limit.
func (s *Store) ListConversations(ctx context.Context, userID int64, limit int) ([]*Conversation, error) {
	out, _, err := s.ListConversationPage(ctx, userID, ConversationFilter{Limit: limit})
	return out, err
}

// ConversationPatch changes some of a conversation's user-settable fields;
// nil fields are left alone.
type ConversationPatch struct {
	// Title, when set, also marks the title as the user's (TitleUser).
	Title    *string
	Pinned   *bool
	Archived *bool
}

// UpdateConversation applies p to a conversation owned by userID and returns
// it. updated_at is bumped.
func (s *Store) UpdateConversation(ctx context.Context, userID int64, id string, p ConversationPatch) (*Conversation, error) {
	set := []string{"updated_at = ?"}
	args := []any{ms(s.now())}
	if p.Title != nil {
		set = append(set, "title = ?", "title_source = ?")
		args = append(args, *p.Title, TitleUser)
	}
	if p.Pinned != nil {
		set = append(set, "pinned = ?")
		args = append(args, *p.Pinned)
	}
	if p.Archived != nil {
		set = append(set, "archived = ?")
		args = append(args, *p.Archived)
	}
	args = append(args, id, userID)
	res, err := s.db.ExecContext(ctx, `UPDATE conversations SET `+strings.Join(set, ", ")+` WHERE id = ? AND user_id = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("update conversation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("update conversation %s: %w", id, ErrNotFound)
	}
	return s.GetConversation(ctx, userID, id)
}

// SetConversationTitle renames a conversation owned by userID (as the user:
// the title source becomes TitleUser).
func (s *Store) SetConversationTitle(ctx context.Context, userID int64, id, title string) error {
	_, err := s.UpdateConversation(ctx, userID, id, ConversationPatch{Title: &title})
	return err
}

// SetAutoTitle sets an automatic title on a conversation owned by userID,
// unless its title source is TitleUser. It reports whether the title was
// changed (false also when the title was already t) and returns the
// conversation as stored afterwards.
func (s *Store) SetAutoTitle(ctx context.Context, userID int64, id, title string) (*Conversation, bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = ?, updated_at = ? WHERE id = ? AND user_id = ? AND title_source = ? AND title <> ?`,
		title, ms(s.now()), id, userID, TitleAuto, title)
	if err != nil {
		return nil, false, fmt.Errorf("set auto title: %w", err)
	}
	n, _ := res.RowsAffected()
	c, err := s.GetConversation(ctx, userID, id)
	if err != nil {
		return nil, false, err
	}
	return c, n > 0, nil
}

// SetConversationUnread sets or clears the unread flag of a conversation
// owned by userID. It reports whether the flag changed and returns the
// conversation as stored afterwards. updated_at is not bumped (reading is
// not an edit).
func (s *Store) SetConversationUnread(ctx context.Context, userID int64, id string, unread bool) (*Conversation, bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE conversations SET unread = ? WHERE id = ? AND user_id = ? AND unread <> ?`,
		unread, id, userID, unread)
	if err != nil {
		return nil, false, fmt.Errorf("set conversation unread: %w", err)
	}
	n, _ := res.RowsAffected()
	c, err := s.GetConversation(ctx, userID, id)
	if err != nil {
		return nil, false, err
	}
	return c, n > 0, nil
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

// AppendMessage appends a message to a conversation and bumps its updated_at
// and last_message_at. The caller must already have verified ownership of
// the conversation.
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
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ?, last_message_at = ? WHERE id = ?`,
		ms(now), ms(now), conversationID); err != nil {
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
