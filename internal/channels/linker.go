package channels

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/audit"
	"github.com/perishabledave/bosun/internal/store"
)

// Link codes.
const (
	// CodeLength is the length of a link code.
	CodeLength = 6
	// DefaultCodeTTL is how long a link code stays valid.
	DefaultCodeTTL = 10 * time.Minute
	// MaxFailedRedeems is how many wrong codes one external identity may
	// send per FailedRedeemWindow before Redeem refuses it outright.
	MaxFailedRedeems   = 5
	FailedRedeemWindow = time.Hour
)

// codeAlphabet omits look-alikes (0/O, 1/I/L) so codes survive being read
// off a terminal and typed on a phone.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// Errors returned by the Linker.
var (
	// ErrNotLinked means the external identity is not bound to any user (or,
	// for Unlink, not to the calling user).
	ErrNotLinked = errors.New("channels: not linked")
	// ErrInvalidCode means the code is unknown, expired, already used, or for
	// another channel.
	ErrInvalidCode = errors.New("channels: invalid or expired link code")
	// ErrAlreadyLinked means the external identity is linked to another user.
	ErrAlreadyLinked = errors.New("channels: already linked to another user")
	// ErrTooManyAttempts means too many wrong codes were sent recently.
	ErrTooManyAttempts = errors.New("channels: too many failed link attempts")
)

// Link binds an external identity on a channel to a Bosun user.
type Link struct {
	Channel        string
	ExternalID     string
	UserID         int64
	DisplayName    string
	ConversationID string // the chat's long-lived conversation; "" until first use
	CreatedAt      time.Time
}

func fromStore(l *store.ChannelLink) Link {
	return Link{Channel: l.Channel, ExternalID: l.ExternalID, UserID: l.UserID, DisplayName: l.DisplayName,
		ConversationID: l.ConversationID, CreatedAt: l.CreatedAt}
}

// LinkStore is the subset of *store.Store the Linker needs.
type LinkStore interface {
	CreateChannelLink(ctx context.Context, l store.ChannelLink) (*store.ChannelLink, error)
	GetChannelLink(ctx context.Context, channel, externalID string) (*store.ChannelLink, error)
	ListChannelLinks(ctx context.Context, userID int64, channel string) ([]*store.ChannelLink, error)
	ListChannelLinksByChannel(ctx context.Context, channel string) ([]*store.ChannelLink, error)
	DeleteChannelLink(ctx context.Context, userID int64, channel, externalID string) error
	SetChannelLinkConversation(ctx context.Context, channel, externalID, conversationID string) error
}

var _ LinkStore = (*store.Store)(nil)

// LinkerOptions configure a Linker.
type LinkerOptions struct {
	// TTL is how long a code is valid (default DefaultCodeTTL).
	TTL time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
	// Audit, if set, records links and unlinks (kind config).
	Audit audit.Logger
	// Log receives operational errors. Nil discards.
	Log *slog.Logger
}

type pendingCode struct {
	userID  int64
	channel string
	expires time.Time
}

type userChannel struct {
	userID  int64
	channel string
}

// Linker maps external identities to Bosun users through one-time link
// codes. Codes live in memory only: a daemon restart invalidates them, which
// is fine for something valid for ten minutes.
type Linker struct {
	st  LinkStore
	ttl time.Duration
	now func() time.Time
	aud audit.Logger
	log *slog.Logger

	mu       sync.Mutex
	codes    map[string]pendingCode // code -> pending
	byUser   map[userChannel]string // the user's outstanding code per channel
	failures map[string][]time.Time // channel + "\x00" + external id -> recent failures
	watchers []func(channel string, userID int64)
}

// NewLinker returns a Linker backed by st.
func NewLinker(st LinkStore, o LinkerOptions) *Linker {
	l := &Linker{st: st, ttl: o.TTL, now: o.Now, aud: o.Audit, log: o.Log,
		codes: map[string]pendingCode{}, byUser: map[userChannel]string{}, failures: map[string][]time.Time{}}
	if l.ttl <= 0 {
		l.ttl = DefaultCodeTTL
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.log == nil {
		l.log = slog.New(slog.DiscardHandler)
	}
	return l
}

// OnChange registers fn to be called (synchronously, after the change is
// stored) whenever a link of userID on channel is created or removed through
// the Linker. Channels use it to start or stop delivering a user's events.
func (l *Linker) OnChange(fn func(channel string, userID int64)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.watchers = append(l.watchers, fn)
}

func (l *Linker) changed(channel string, userID int64) {
	l.mu.Lock()
	ws := append([]func(string, int64){}, l.watchers...)
	l.mu.Unlock()
	for _, fn := range ws {
		fn(channel, userID)
	}
}

func randomCode() (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(codeAlphabet)))
	for range CodeLength {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(codeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// purge drops expired codes. l.mu must be held.
func (l *Linker) purge(now time.Time) {
	for c, p := range l.codes {
		if !now.Before(p.expires) {
			delete(l.codes, c)
			if l.byUser[userChannel{p.userID, p.channel}] == c {
				delete(l.byUser, userChannel{p.userID, p.channel})
			}
		}
	}
}

// NewCode issues a one-time code that links the identity redeeming it on
// channel to userID. Issuing a new code invalidates the user's previous
// outstanding code for the same channel.
func (l *Linker) NewCode(ctx context.Context, userID int64, channel string) (string, time.Time, error) {
	if userID <= 0 || channel == "" {
		return "", time.Time{}, fmt.Errorf("channels: new code: invalid user or channel")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.purge(now)
	key := userChannel{userID, channel}
	if old, ok := l.byUser[key]; ok {
		delete(l.codes, old)
	}
	var code string
	for {
		c, err := randomCode()
		if err != nil {
			return "", time.Time{}, fmt.Errorf("channels: new code: %w", err)
		}
		if _, taken := l.codes[c]; !taken {
			code = c
			break
		}
	}
	exp := now.Add(l.ttl)
	l.codes[code] = pendingCode{userID: userID, channel: channel, expires: exp}
	l.byUser[key] = code
	return code, exp, nil
}

// NormalizeCode upper-cases a code and strips spaces and dashes, so
// "abc 123" and "ABC-123" both match.
func NormalizeCode(code string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '-' || r == '\t':
			return -1
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		}
		return r
	}, code)
}

// Redeem consumes code and binds (channel, externalID) to the code's user.
// The code is single-use: it is consumed even if linking then fails. If the
// identity is already linked to the same user, the existing link is
// returned; if it is linked to someone else, ErrAlreadyLinked. Repeated
// wrong codes from one identity are refused with ErrTooManyAttempts.
func (l *Linker) Redeem(ctx context.Context, channel, code, externalID, displayName string) (Link, error) {
	if channel == "" || externalID == "" {
		return Link{}, ErrInvalidCode
	}
	code = NormalizeCode(code)
	failKey := channel + "\x00" + externalID
	l.mu.Lock()
	now := l.now()
	l.purge(now)
	recent := l.failures[failKey][:0]
	for _, t := range l.failures[failKey] {
		if now.Sub(t) < FailedRedeemWindow {
			recent = append(recent, t)
		}
	}
	l.failures[failKey] = recent
	if len(recent) >= MaxFailedRedeems {
		l.mu.Unlock()
		return Link{}, ErrTooManyAttempts
	}
	p, ok := l.codes[code]
	if !ok || p.channel != channel {
		l.failures[failKey] = append(l.failures[failKey], now)
		l.mu.Unlock()
		return Link{}, ErrInvalidCode
	}
	delete(l.codes, code)
	delete(l.byUser, userChannel{p.userID, p.channel})
	delete(l.failures, failKey)
	l.mu.Unlock()

	if existing, err := l.st.GetChannelLink(ctx, channel, externalID); err == nil {
		if existing.UserID != p.userID {
			return Link{}, ErrAlreadyLinked
		}
		return fromStore(existing), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return Link{}, fmt.Errorf("channels: redeem: %w", err)
	}
	row, err := l.st.CreateChannelLink(ctx, store.ChannelLink{Channel: channel, ExternalID: externalID, UserID: p.userID, DisplayName: displayName})
	if errors.Is(err, store.ErrConflict) {
		return Link{}, ErrAlreadyLinked // lost a race with another redeem
	}
	if err != nil {
		return Link{}, fmt.Errorf("channels: redeem: %w", err)
	}
	l.record(ctx, p.userID, channel, externalID, "channel linked")
	l.log.Info("channel linked", "channel", channel, "external_id", externalID, "user_id", p.userID)
	l.changed(channel, p.userID)
	return fromStore(row), nil
}

// Lookup returns the link for (channel, externalID) or ErrNotLinked.
func (l *Linker) Lookup(ctx context.Context, channel, externalID string) (Link, error) {
	row, err := l.st.GetChannelLink(ctx, channel, externalID)
	if errors.Is(err, store.ErrNotFound) {
		return Link{}, ErrNotLinked
	}
	if err != nil {
		return Link{}, fmt.Errorf("channels: lookup: %w", err)
	}
	return fromStore(row), nil
}

// Links returns userID's links, optionally limited to one channel ("" = all).
func (l *Linker) Links(ctx context.Context, userID int64, channel string) ([]Link, error) {
	rows, err := l.st.ListChannelLinks(ctx, userID, channel)
	if err != nil {
		return nil, fmt.Errorf("channels: links: %w", err)
	}
	out := make([]Link, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromStore(r))
	}
	return out, nil
}

// ChannelLinks returns every user's links on channel.
func (l *Linker) ChannelLinks(ctx context.Context, channel string) ([]Link, error) {
	rows, err := l.st.ListChannelLinksByChannel(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("channels: links: %w", err)
	}
	out := make([]Link, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromStore(r))
	}
	return out, nil
}

// Unlink removes userID's link for (channel, externalID). ErrNotLinked if
// the user has no such link.
func (l *Linker) Unlink(ctx context.Context, userID int64, channel, externalID string) error {
	err := l.st.DeleteChannelLink(ctx, userID, channel, externalID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotLinked
	}
	if err != nil {
		return fmt.Errorf("channels: unlink: %w", err)
	}
	l.record(ctx, userID, channel, externalID, "channel unlinked")
	l.log.Info("channel unlinked", "channel", channel, "external_id", externalID, "user_id", userID)
	l.changed(channel, userID)
	return nil
}

// SetConversation records the conversation the linked identity talks to.
func (l *Linker) SetConversation(ctx context.Context, channel, externalID, conversationID string) error {
	err := l.st.SetChannelLinkConversation(ctx, channel, externalID, conversationID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotLinked
	}
	return err
}

func (l *Linker) record(ctx context.Context, userID int64, channel, externalID, note string) {
	if l.aud == nil {
		return
	}
	detail, _ := json.Marshal(struct {
		Channel    string `json:"channel"`
		ExternalID string `json:"external_id"`
	}{channel, externalID})
	e := audit.Entry{UserID: userID, Kind: audit.KindConfig, Integration: CredentialIntegration, Notes: []string{note}, Detail: detail}
	if err := l.aud.Record(context.WithoutCancel(ctx), e); err != nil {
		l.log.Error("channels: audit failed", "err", err)
	}
}
