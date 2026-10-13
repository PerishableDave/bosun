package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dperedo/bosun/internal/channels"
)

// Channels is what the channel endpoints need (usually *channels.Manager).
// Every method is scoped to the calling user.
type Channels interface {
	// Names lists the enabled channels.
	Names() []string
	NewLinkCode(ctx context.Context, userID int64, channel string) (channels.LinkCode, error)
	Links(ctx context.Context, userID int64) ([]channels.Link, error)
	Unlink(ctx context.Context, userID int64, channel, externalID string) error
}

var _ Channels = (*channels.Manager)(nil)

// ChannelLinkCodeResponse is the body of POST /v1/channels/{channel}/link.
type ChannelLinkCodeResponse struct {
	Channel   string    `json:"channel"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	// Instructions tell the user what to send where, e.g.
	// "Send /link ABC234 to @MyBosunBot on Telegram."
	Instructions string `json:"instructions"`
}

// ChannelLinkResponse describes one linked external identity.
type ChannelLinkResponse struct {
	Channel        string    `json:"channel"`
	ExternalID     string    `json:"external_id"`
	DisplayName    string    `json:"display_name,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// ChannelListResponse is the body of GET /v1/channels.
type ChannelListResponse struct {
	// Channels are the channels enabled on this server.
	Channels []string `json:"channels"`
	// Links are the caller's linked identities.
	Links []ChannelLinkResponse `json:"links"`
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	if s.channels == nil {
		writeJSON(w, http.StatusOK, ChannelListResponse{Channels: []string{}, Links: []ChannelLinkResponse{}})
		return
	}
	u, _ := UserFromContext(r.Context())
	links, err := s.channels.Links(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, "list channel links", err)
		return
	}
	out := ChannelListResponse{Channels: s.channels.Names(), Links: make([]ChannelLinkResponse, 0, len(links))}
	for _, l := range links {
		out.Links = append(out.Links, ChannelLinkResponse{Channel: l.Channel, ExternalID: l.ExternalID,
			DisplayName: l.DisplayName, ConversationID: l.ConversationID, CreatedAt: l.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleChannelLinkCode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("channel")
	if s.channels == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("channel %q is not enabled", name))
		return
	}
	u, _ := UserFromContext(r.Context())
	lc, err := s.channels.NewLinkCode(r.Context(), u.ID, name)
	if errors.Is(err, channels.ErrUnknownChannel) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("channel %q is not enabled", name))
		return
	}
	if err != nil {
		s.internalError(w, r, "new link code", err)
		return
	}
	writeJSON(w, http.StatusCreated, ChannelLinkCodeResponse{Channel: lc.Channel, Code: lc.Code,
		ExpiresAt: lc.ExpiresAt.UTC(), Instructions: lc.Instructions})
}

func (s *Server) handleDeleteChannelLink(w http.ResponseWriter, r *http.Request) {
	if s.channels == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such link")
		return
	}
	u, _ := UserFromContext(r.Context())
	err := s.channels.Unlink(r.Context(), u.ID, r.PathValue("channel"), r.PathValue("external_id"))
	if errors.Is(err, channels.ErrNotLinked) {
		writeError(w, http.StatusNotFound, "not_found", "no such link")
		return
	}
	if err != nil {
		s.internalError(w, r, "unlink channel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
