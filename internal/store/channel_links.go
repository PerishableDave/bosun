package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ChannelLink is a channel_links row: an external chat identity (e.g. a
// Telegram chat ID) bound to a Bosun user.
type ChannelLink struct {
	ID             int64
	Channel        string // "telegram"
	ExternalID     string // channel-specific identity, e.g. the chat ID
	UserID         int64
	DisplayName    string
	ConversationID string // "" until the chat's first message
	CreatedAt      time.Time
}

const channelLinkCols = `id, channel, external_id, user_id, display_name, conversation_id, created_at`

func scanChannelLink(r interface{ Scan(...any) error }) (*ChannelLink, error) {
	var l ChannelLink
	var conv sql.NullString
	var cr int64
	if err := r.Scan(&l.ID, &l.Channel, &l.ExternalID, &l.UserID, &l.DisplayName, &conv, &cr); err != nil {
		return nil, notFound(err)
	}
	l.ConversationID, l.CreatedAt = conv.String, fromMS(cr)
	return &l, nil
}

func (s *Store) queryChannelLinks(ctx context.Context, q string, args ...any) ([]*ChannelLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelLinkCols+` FROM channel_links `+q, args...)
	if err != nil {
		return nil, fmt.Errorf("list channel links: %w", err)
	}
	defer rows.Close()
	var out []*ChannelLink
	for rows.Next() {
		l, err := scanChannelLink(rows)
		if err != nil {
			return nil, fmt.Errorf("list channel links: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CreateChannelLink inserts a link. ErrConflict if (channel, external_id) is
// already linked.
func (s *Store) CreateChannelLink(ctx context.Context, l ChannelLink) (*ChannelLink, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO channel_links(channel, external_id, user_id, display_name, conversation_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		l.Channel, l.ExternalID, l.UserID, l.DisplayName, nullStr(l.ConversationID), ms(now))
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("create channel link: %w", ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("create channel link: %w", err)
	}
	l.ID, _ = res.LastInsertId()
	l.CreatedAt = fromMS(ms(now))
	return &l, nil
}

// GetChannelLink returns the link for (channel, externalID) or ErrNotFound.
func (s *Store) GetChannelLink(ctx context.Context, channel, externalID string) (*ChannelLink, error) {
	l, err := scanChannelLink(s.db.QueryRowContext(ctx,
		`SELECT `+channelLinkCols+` FROM channel_links WHERE channel = ? AND external_id = ?`, channel, externalID))
	if err != nil {
		return nil, fmt.Errorf("get channel link: %w", err)
	}
	return l, nil
}

// ListChannelLinks returns userID's links, optionally limited to one channel
// ("" = all), oldest first.
func (s *Store) ListChannelLinks(ctx context.Context, userID int64, channel string) ([]*ChannelLink, error) {
	if channel == "" {
		return s.queryChannelLinks(ctx, `WHERE user_id = ? ORDER BY channel, created_at, id`, userID)
	}
	return s.queryChannelLinks(ctx, `WHERE user_id = ? AND channel = ? ORDER BY created_at, id`, userID, channel)
}

// ListChannelLinksByChannel returns every user's links for one channel.
func (s *Store) ListChannelLinksByChannel(ctx context.Context, channel string) ([]*ChannelLink, error) {
	return s.queryChannelLinks(ctx, `WHERE channel = ? ORDER BY user_id, created_at, id`, channel)
}

// DeleteChannelLink removes userID's link for (channel, externalID).
// ErrNotFound if there is none (including when it belongs to another user).
func (s *Store) DeleteChannelLink(ctx context.Context, userID int64, channel, externalID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM channel_links WHERE user_id = ? AND channel = ? AND external_id = ?`,
		userID, channel, externalID)
	if err != nil {
		return fmt.Errorf("delete channel link: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete channel link: %w", ErrNotFound)
	}
	return nil
}

// SetChannelLinkConversation records the conversation a linked chat talks
// to ("" clears it).
func (s *Store) SetChannelLinkConversation(ctx context.Context, channel, externalID, conversationID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE channel_links SET conversation_id = ? WHERE channel = ? AND external_id = ?`,
		nullStr(conversationID), channel, externalID)
	if err != nil {
		return fmt.Errorf("set channel link conversation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set channel link conversation: %w", ErrNotFound)
	}
	return nil
}
