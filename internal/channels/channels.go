// Package channels is the framework for chat-app frontends ("text your
// assistant"): Telegram today, Slack/Discord/iMessage later.
//
// A Channel is a long-running connector that receives messages from an
// external chat service and turns them into agent turns for the Bosun user
// the sender is linked to. Identity is never inferred: an external identity
// (e.g. a Telegram chat) acts as a Bosun user only after that user issued a
// one-time code (POST /v1/channels/{channel}/link, `bosun channels link`) and
// the code was sent from inside the chat app (Linker.Redeem). Messages from
// unlinked identities never reach the agent.
//
// The Manager holds the configured channels, starts and stops them with the
// daemon, and backs the /v1/channels API.
package channels

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/integration"
)

// CredentialIntegration is the vault integration name channel secrets are
// stored under (e.g. the shared credential channels/telegram_bot_token).
const CredentialIntegration = "channels"

// Channel is one chat-app frontend.
type Channel interface {
	// Name is the channel's identifier in links and URLs ("telegram");
	// it must match ^[a-z][a-z0-9_-]*$.
	Name() string
	// Start begins the channel's background work and returns promptly;
	// the work runs until ctx ends or Close is called. Missing
	// configuration (e.g. no bot token yet) is not an error: the channel
	// waits for it and reports its state through Status.
	Start(ctx context.Context) error
	// Close stops the channel and waits for its goroutines. In-flight turns
	// are canceled.
	Close() error
}

// LinkInstructor is optionally implemented by channels to tell the user
// what to do with a link code ("Send /link ABC123 to @MyBosunBot").
type LinkInstructor interface {
	LinkInstructions(code string) string
}

// CredentialUser is optionally implemented by channels that read secrets
// from the vault under CredentialIntegration; it lists the credential kinds
// (e.g. "telegram_bot_token") so the credentials API accepts them.
type CredentialUser interface {
	CredentialKinds() []string
}

// ErrUnknownChannel is returned for a channel name that is not enabled.
var ErrUnknownChannel = errors.New("channels: unknown channel")

// LinkCode is a freshly issued link code.
type LinkCode struct {
	Channel      string
	Code         string
	ExpiresAt    time.Time
	Instructions string
}

// Manager owns the enabled channels and the Linker.
type Manager struct {
	linker *Linker
	log    *slog.Logger

	mu    sync.Mutex
	chans []Channel
}

// NewManager returns an empty Manager.
func NewManager(l *Linker, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{linker: l, log: log}
}

// Linker returns the manager's Linker.
func (m *Manager) Linker() *Linker { return m.linker }

// Add registers a channel. Names must be unique.
func (m *Manager) Add(ch Channel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.chans {
		if c.Name() == ch.Name() {
			return fmt.Errorf("channels: duplicate channel %q", ch.Name())
		}
	}
	m.chans = append(m.chans, ch)
	return nil
}

// Get returns the channel named name.
func (m *Manager) Get(name string) (Channel, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.chans {
		if c.Name() == name {
			return c, true
		}
	}
	return nil, false
}

// Names returns the enabled channels' names, sorted.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.chans))
	for _, c := range m.chans {
		out = append(out, c.Name())
	}
	sort.Strings(out)
	return out
}

// Start starts every channel. A channel that fails to start is logged and
// skipped; the others keep running.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	chans := append([]Channel(nil), m.chans...)
	m.mu.Unlock()
	for _, c := range chans {
		if err := c.Start(ctx); err != nil {
			m.log.Error("channel failed to start", "channel", c.Name(), "err", err)
			continue
		}
		m.log.Info("channel started", "channel", c.Name())
	}
}

// Close closes every channel in reverse order.
func (m *Manager) Close() error {
	m.mu.Lock()
	chans := append([]Channel(nil), m.chans...)
	m.mu.Unlock()
	var errs []error
	for i := len(chans) - 1; i >= 0; i-- {
		if err := chans[i].Close(); err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", chans[i].Name(), err))
		}
	}
	return errors.Join(errs...)
}

// NewLinkCode issues a link code for userID on an enabled channel.
func (m *Manager) NewLinkCode(ctx context.Context, userID int64, channel string) (LinkCode, error) {
	ch, ok := m.Get(channel)
	if !ok {
		return LinkCode{}, ErrUnknownChannel
	}
	code, exp, err := m.linker.NewCode(ctx, userID, channel)
	if err != nil {
		return LinkCode{}, err
	}
	lc := LinkCode{Channel: channel, Code: code, ExpiresAt: exp}
	if li, ok := ch.(LinkInstructor); ok {
		lc.Instructions = li.LinkInstructions(code)
	} else {
		lc.Instructions = fmt.Sprintf("Send /link %s from %s.", code, channel)
	}
	return lc, nil
}

// Links returns userID's links on every channel (including channels that
// are no longer enabled, so they can still be removed).
func (m *Manager) Links(ctx context.Context, userID int64) ([]Link, error) {
	return m.linker.Links(ctx, userID, "")
}

// Unlink removes one of userID's links.
func (m *Manager) Unlink(ctx context.Context, userID int64, channel, externalID string) error {
	return m.linker.Unlink(ctx, userID, channel, externalID)
}

// Integration returns a tool-less integration named CredentialIntegration
// that declares the channels' credential kinds (so `bosun credentials set
// channels <kind> --shared` is accepted) and reports their health in
// GET /v1/integrations. It is never registered with the tool registry.
func (m *Manager) Integration() integration.Integration { return credIntegration{m} }

type credIntegration struct{ m *Manager }

func (c credIntegration) Manifest() integration.Manifest {
	var kinds []string
	c.m.mu.Lock()
	for _, ch := range c.m.chans {
		if cu, ok := ch.(CredentialUser); ok {
			kinds = append(kinds, cu.CredentialKinds()...)
		}
	}
	c.m.mu.Unlock()
	sort.Strings(kinds)
	return integration.Manifest{Name: CredentialIntegration, Version: "1", Scope: integration.ScopeShared, CredentialKinds: kinds}
}

func (credIntegration) Init(context.Context, integration.Config, integration.CredentialSource) error {
	return nil
}
func (credIntegration) Tools() []integration.Tool { return nil }
func (credIntegration) Close() error              { return nil }

// ChannelStatus is one channel's entry in the integration status details.
type ChannelStatus struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// Status reports the worst state among the channels that report one.
func (c credIntegration) Status() integration.Status {
	c.m.mu.Lock()
	chans := append([]Channel(nil), c.m.chans...)
	c.m.mu.Unlock()
	rank := map[string]int{integration.StatusOK: 0, integration.StatusDegraded: 1, integration.StatusDown: 2}
	out := integration.Status{State: integration.StatusOK}
	var details []ChannelStatus
	for _, ch := range chans {
		st := integration.Status{State: integration.StatusOK}
		if sr, ok := ch.(integration.StatusReporter); ok {
			st = sr.Status()
		}
		details = append(details, ChannelStatus{Name: ch.Name(), State: st.State, Message: st.Message})
		if rank[st.State] > rank[out.State] {
			out.State = st.State
		}
		if st.Message != "" {
			if out.Message != "" {
				out.Message += "; "
			}
			out.Message += ch.Name() + ": " + st.Message
		}
	}
	out.Details = details
	return out
}
