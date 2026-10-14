// Package telegram is Bosun's Telegram channel: a bot that long-polls the
// Bot API (getUpdates), so it needs no public URL or webhook, and lets
// linked users talk to their assistant and answer approvals from their
// phone.
//
// Trust model: a private chat that has been linked (channels.Linker) acts
// as its Bosun user, exactly like the CLI. Everything else is refused: the
// agent never runs for an unlinked chat, group chats are ignored, and every
// update's sender must be the linked chat itself.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/perishabledave/bosun/internal/api/events"
	"github.com/perishabledave/bosun/internal/approval"
	"github.com/perishabledave/bosun/internal/channels"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/store"
)

// Channel identity.
const (
	// Name is the channel name used in links and URLs.
	Name = "telegram"
	// CredentialKind is the shared vault credential holding the bot token
	// (integration channels.CredentialIntegration).
	CredentialKind = "telegram_bot_token"
	// ConversationTitle is the title of conversations created for a chat.
	ConversationTitle = "telegram"
)

// Defaults for Options.
const (
	DefaultPollTimeout           = 30 * time.Second
	DefaultEditInterval          = time.Second
	DefaultTokenRetry            = 30 * time.Second
	DefaultUnlinkedReplyInterval = time.Hour
)

// eventBuffer is the per-user bus buffer. Text deltas arrive faster than
// Telegram calls complete, so it is generous.
const eventBuffer = 2048

// Runner runs one agent turn (usually *agent.Agent).
type Runner interface {
	Run(ctx context.Context, userID int64, conversationID, userText string) error
}

// Store is the subset of *store.Store the bot needs.
type Store interface {
	GetUser(ctx context.Context, id int64) (*store.User, error)
	CreateConversation(ctx context.Context, userID int64, title string, routineID int64) (*store.Conversation, error)
	GetConversation(ctx context.Context, userID int64, id string) (*store.Conversation, error)
	GetRoutine(ctx context.Context, userID, id int64) (*store.Routine, error)
	GetRoutineRun(ctx context.Context, userID, runID int64) (*store.RoutineRun, error)
}

var _ Store = (*store.Store)(nil)

// Approvals is the subset of approval.Manager the bot needs.
type Approvals interface {
	Resolve(ctx context.Context, id string, userID int64, d approval.Decision) error
	ListPending(ctx context.Context, userID int64) ([]*approval.Approval, error)
}

// TokenFunc returns the bot token; "" with a nil error means "not
// configured yet" (the bot keeps checking).
type TokenFunc func(ctx context.Context) (string, error)

// Options configure a Bot. Token, Linker, Bus, Runner, Store and Approvals
// are required.
type Options struct {
	Token     TokenFunc
	Linker    *channels.Linker
	Bus       *events.Bus
	Runner    Runner
	Store     Store
	Approvals Approvals
	Log       *slog.Logger

	// APIURL is the Bot API base URL (default DefaultAPIURL).
	APIURL string
	// AllowedUsernames, if non-empty, restricts which Telegram usernames
	// may link a chat (case-insensitive, leading "@" optional).
	AllowedUsernames []string
	// HTTPClient is used for Bot API calls (default: a new client).
	HTTPClient *http.Client

	// Tuning; zero means the default.
	PollTimeout           time.Duration
	EditInterval          time.Duration // minimum time between edits of one message
	TokenRetry            time.Duration // how often to re-check a missing or rejected token
	UnlinkedReplyInterval time.Duration // one help reply per unlinked chat per interval
}

// Bot is the Telegram channel. It implements channels.Channel.
type Bot struct {
	o       Options
	log     *slog.Logger
	allowed map[string]bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	client    *Client
	me        *User
	state     string
	statusMsg string
	watchers  map[int64]*watcher       // by Bosun user ID
	approvals map[string]*approvalPost // by approval ID
	unlinked  map[int64]time.Time      // chat ID -> last help reply
	closed    bool
}

var (
	_ channels.Channel        = (*Bot)(nil)
	_ channels.LinkInstructor = (*Bot)(nil)
	_ channels.CredentialUser = (*Bot)(nil)
)

// New returns a Bot; call Start to run it.
func New(o Options) (*Bot, error) {
	if o.Token == nil || o.Linker == nil || o.Bus == nil || o.Runner == nil || o.Store == nil || o.Approvals == nil {
		return nil, errors.New("telegram: Token, Linker, Bus, Runner, Store and Approvals are required")
	}
	if o.PollTimeout <= 0 {
		o.PollTimeout = DefaultPollTimeout
	}
	if o.EditInterval <= 0 {
		o.EditInterval = DefaultEditInterval
	}
	if o.TokenRetry <= 0 {
		o.TokenRetry = DefaultTokenRetry
	}
	if o.UnlinkedReplyInterval <= 0 {
		o.UnlinkedReplyInterval = DefaultUnlinkedReplyInterval
	}
	b := &Bot{o: o, log: o.Log, allowed: map[string]bool{}, state: integration.StatusDown, statusMsg: "not started",
		watchers: map[int64]*watcher{}, approvals: map[string]*approvalPost{}, unlinked: map[int64]time.Time{}}
	if b.log == nil {
		b.log = slog.New(slog.DiscardHandler)
	}
	for _, u := range o.AllowedUsernames {
		if u = normUsername(u); u != "" {
			b.allowed[u] = true
		}
	}
	return b, nil
}

func normUsername(u string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(u), "@"))
}

// Name implements channels.Channel.
func (b *Bot) Name() string { return Name }

// CredentialKinds implements channels.CredentialUser.
func (b *Bot) CredentialKinds() []string { return []string{CredentialKind} }

// LinkInstructions implements channels.LinkInstructor.
func (b *Bot) LinkInstructions(code string) string {
	b.mu.Lock()
	me := b.me
	b.mu.Unlock()
	if me != nil && me.Username != "" {
		return fmt.Sprintf("Send /link %s to @%s on Telegram.", code, me.Username)
	}
	return fmt.Sprintf("Send /link %s to your Bosun bot on Telegram.", code)
}

// Status implements integration.StatusReporter.
func (b *Bot) Status() integration.Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return integration.Status{State: b.state, Message: b.statusMsg}
}

func (b *Bot) setStatus(state, msg string) {
	b.mu.Lock()
	changed := b.state != state || b.statusMsg != msg
	b.state, b.statusMsg = state, msg
	b.mu.Unlock()
	if changed {
		b.log.Info("telegram: status", "state", state, "message", msg)
	}
}

// Start implements channels.Channel.
func (b *Bot) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx != nil {
		return errors.New("telegram: already started")
	}
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.o.Linker.OnChange(b.linksChanged)
	b.wg.Add(1)
	go b.run()
	return nil
}

// Close implements channels.Channel.
func (b *Bot) Close() error {
	b.mu.Lock()
	if b.closed || b.cancel == nil {
		b.closed = true
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.cancel()
	ws := make([]*watcher, 0, len(b.watchers))
	for id, w := range b.watchers {
		ws = append(ws, w)
		delete(b.watchers, id)
	}
	b.mu.Unlock()
	for _, w := range ws {
		w.close()
	}
	b.wg.Wait()
	return nil
}

func (b *Bot) api() *Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client
}

// sleep waits d or until the bot stops; it reports whether to continue.
func (b *Bot) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-b.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// run obtains the token, verifies it with getMe, and polls; it starts over
// when the token is missing or rejected.
func (b *Bot) run() {
	defer b.wg.Done()
	for b.ctx.Err() == nil {
		tok, err := b.o.Token(b.ctx)
		if err != nil {
			b.setStatus(integration.StatusDown, "cannot read bot token: "+err.Error())
			if !b.sleep(b.o.TokenRetry) {
				return
			}
			continue
		}
		if tok == "" {
			b.setStatus(integration.StatusDown, "no bot token: set it with `bosun credentials set channels "+CredentialKind+" --shared`")
			if !b.sleep(b.o.TokenRetry) {
				return
			}
			continue
		}
		c := NewClient(b.o.APIURL, tok, b.o.HTTPClient)
		me, err := c.GetMe(b.ctx)
		if err != nil {
			if b.ctx.Err() != nil {
				return
			}
			var ae *APIError
			if errors.As(err, &ae) && ae.Unauthorized() {
				b.setStatus(integration.StatusDown, "Telegram rejected the bot token")
			} else {
				b.setStatus(integration.StatusDegraded, "cannot reach Telegram: "+err.Error())
			}
			if !b.sleep(b.o.TokenRetry) {
				return
			}
			continue
		}
		b.mu.Lock()
		b.client, b.me = c, me
		b.mu.Unlock()
		b.setStatus(integration.StatusOK, "polling as @"+me.Username)
		b.watchLinkedUsers()
		b.poll(c)
	}
}

// watchLinkedUsers subscribes to the events of every user with a link.
func (b *Bot) watchLinkedUsers() {
	links, err := b.o.Linker.ChannelLinks(b.ctx, Name)
	if err != nil {
		b.log.Error("telegram: list links", "err", err)
		return
	}
	for _, l := range links {
		b.watcher(l.UserID)
	}
}

// poll long-polls getUpdates until the bot stops or the token is rejected.
func (b *Bot) poll(c *Client) {
	var offset int64
	backoff := time.Second
	for b.ctx.Err() == nil {
		ups, err := c.GetUpdates(b.ctx, offset, b.o.PollTimeout)
		if err != nil {
			if b.ctx.Err() != nil {
				return
			}
			var ae *APIError
			if errors.As(err, &ae) && ae.Unauthorized() {
				b.setStatus(integration.StatusDown, "Telegram rejected the bot token")
				b.sleep(b.o.TokenRetry)
				return
			}
			msg := "getUpdates failed: " + err.Error()
			if errors.As(err, &ae) && ae.Code == http.StatusConflict {
				msg = "another client is polling this bot, or a webhook is set (Bosun uses getUpdates)"
			}
			b.setStatus(integration.StatusDegraded, msg)
			if !b.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		b.mu.Lock()
		user := b.me.Username
		b.mu.Unlock()
		b.setStatus(integration.StatusOK, "polling as @"+user)
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			b.handleUpdate(c, u)
		}
	}
}

// --- incoming updates ---

func (b *Bot) handleUpdate(c *Client, u Update) {
	defer func() {
		if p := recover(); p != nil {
			b.log.Error("telegram: panic handling update", "update_id", u.UpdateID, "panic", fmt.Sprint(p))
		}
	}()
	if u.CallbackQuery != nil {
		b.handleCallback(c, u.CallbackQuery)
		return
	}
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot {
		return
	}
	cmd, arg := parseCommand(m.Text)
	if m.Chat.Type != "private" {
		// v1: only private chats. A /link attempt gets an explanation;
		// everything else in groups is ignored silently.
		b.log.Info("telegram: ignoring message from non-private chat", "chat_id", m.Chat.ID, "chat_type", m.Chat.Type)
		if cmd == "link" || cmd == "start" {
			b.replyRateLimited(c, m.Chat.ID, "Bosun only works in private chats. Message me directly to link your account.")
		}
		return
	}
	if m.From.ID != m.Chat.ID {
		b.log.Warn("telegram: sender does not match private chat; ignoring", "chat_id", m.Chat.ID)
		return
	}
	extID := strconv.FormatInt(m.Chat.ID, 10)
	link, err := b.o.Linker.Lookup(b.ctx, Name, extID)
	if errors.Is(err, channels.ErrNotLinked) {
		if cmd == "link" {
			b.handleLink(c, m, arg)
			return
		}
		b.log.Info("telegram: message from unlinked chat", "chat_id", m.Chat.ID)
		b.replyRateLimited(c, m.Chat.ID, unlinkedHelp)
		return
	}
	if err != nil {
		b.log.Error("telegram: link lookup failed", "chat_id", m.Chat.ID, "err", err)
		return
	}
	user, err := b.o.Store.GetUser(b.ctx, link.UserID)
	if err != nil {
		b.log.Error("telegram: linked user lookup failed", "chat_id", m.Chat.ID, "user_id", link.UserID, "err", err)
		return
	}
	if user.Disabled {
		b.log.Info("telegram: message for disabled user", "chat_id", m.Chat.ID, "user_id", user.ID)
		b.replyRateLimited(c, m.Chat.ID, "This Bosun account is disabled.")
		return
	}
	chatID := m.Chat.ID
	switch cmd {
	case "":
		if strings.TrimSpace(m.Text) == "" {
			b.sendPlain(c, chatID, "I can only read text messages for now.")
			return
		}
		b.startTurn(c, link, chatID, m.Text)
	case "start", "help":
		b.sendPlain(c, chatID, linkedHelp)
	case "link":
		b.sendPlain(c, chatID, fmt.Sprintf("This chat is already linked to Bosun user %s. Send /unlink first to link it to another account.", user.Username))
	case "unlink":
		if err := b.o.Linker.Unlink(b.ctx, user.ID, Name, extID); err != nil {
			b.log.Error("telegram: unlink failed", "chat_id", chatID, "err", err)
			b.sendPlain(c, chatID, "Unlinking failed; try `bosun channels unlink` on your computer.")
			return
		}
		b.sendPlain(c, chatID, "Unlinked. This chat can no longer talk to your assistant.")
	case "whoami":
		conv := link.ConversationID
		if conv == "" {
			conv = "(none yet)"
		}
		b.sendPlain(c, chatID, fmt.Sprintf("You are %s on Bosun (linked %s).\nConversation: %s",
			user.Username, link.CreatedAt.Local().Format("2006-01-02"), conv))
	case "new":
		conv, err := b.newConversation(link)
		if err != nil {
			b.log.Error("telegram: new conversation failed", "chat_id", chatID, "err", err)
			b.sendPlain(c, chatID, "Could not start a new conversation.")
			return
		}
		b.log.Info("telegram: new conversation", "chat_id", chatID, "user_id", user.ID, "conversation_id", conv)
		b.sendPlain(c, chatID, "Started a new conversation.")
	case "approvals":
		b.listApprovals(c, user.ID, chatID)
	default:
		b.sendPlain(c, chatID, "Unknown command /"+cmd+". Send /help for the list.")
	}
}

const unlinkedHelp = "This is a private Bosun assistant. If it is yours, link this chat: run `bosun channels link telegram` " +
	"on your computer, then send /link CODE here."

const linkedHelp = `Send me a message and your Bosun assistant will answer, with the same permissions as the CLI. Actions that need approval show Approve/Deny buttons.

/new - start a fresh conversation
/approvals - list approvals waiting for you
/whoami - show the linked Bosun account
/unlink - unlink this chat
/help - this message`

// parseCommand splits "/cmd@bot args" into ("cmd", "args"). Non-commands
// return ("", "").
func parseCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	text = text[1:]
	word, rest := text, ""
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		word, rest = text[:i], text[i:]
	}
	word, _, _ = strings.Cut(word, "@")
	return strings.ToLower(word), strings.TrimSpace(rest)
}

func displayName(u *User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if u.Username != "" {
		if name != "" {
			return name + " (@" + u.Username + ")"
		}
		return "@" + u.Username
	}
	return name
}

func (b *Bot) handleLink(c *Client, m *Message, code string) {
	chatID := m.Chat.ID
	if code == "" {
		b.sendPlain(c, chatID, "Usage: /link CODE. Get a code by running `bosun channels link telegram` on your computer.")
		return
	}
	if len(b.allowed) > 0 && !b.allowed[normUsername(m.From.Username)] {
		b.log.Info("telegram: link refused, username not allowed", "chat_id", chatID)
		b.replyRateLimited(c, chatID, "This Telegram account is not allowed to link to this Bosun. "+
			"Ask the admin to add your username to channels.telegram.allowed_usernames.")
		return
	}
	link, err := b.o.Linker.Redeem(b.ctx, Name, code, strconv.FormatInt(chatID, 10), displayName(m.From))
	switch {
	case err == nil:
		user, uerr := b.o.Store.GetUser(b.ctx, link.UserID)
		name := "your account"
		if uerr == nil {
			name = user.Username
		}
		b.log.Info("telegram: chat linked", "chat_id", chatID, "user_id", link.UserID)
		b.sendPlain(c, chatID, fmt.Sprintf("Linked to Bosun user %s. Send me a message to talk to your assistant; /help lists commands.", name))
	case errors.Is(err, channels.ErrInvalidCode):
		b.log.Info("telegram: invalid link code", "chat_id", chatID)
		b.sendPlain(c, chatID, "That code is invalid or expired. Run `bosun channels link telegram` for a new one.")
	case errors.Is(err, channels.ErrTooManyAttempts):
		b.log.Info("telegram: link attempts rate limited", "chat_id", chatID)
		b.replyRateLimited(c, chatID, "Too many failed attempts. Try again later.")
	case errors.Is(err, channels.ErrAlreadyLinked):
		b.sendPlain(c, chatID, "This chat is already linked to another Bosun user.")
	default:
		b.log.Error("telegram: redeem failed", "chat_id", chatID, "err", err)
		b.sendPlain(c, chatID, "Linking failed; try again.")
	}
}

// replyRateLimited sends text to a chat that is not (usefully) linked, at
// most once per UnlinkedReplyInterval per chat.
func (b *Bot) replyRateLimited(c *Client, chatID int64, text string) {
	now := time.Now()
	b.mu.Lock()
	if last, ok := b.unlinked[chatID]; ok && now.Sub(last) < b.o.UnlinkedReplyInterval {
		b.mu.Unlock()
		return
	}
	if len(b.unlinked) > 10000 {
		for id, t := range b.unlinked {
			if now.Sub(t) >= b.o.UnlinkedReplyInterval {
				delete(b.unlinked, id)
			}
		}
	}
	b.unlinked[chatID] = now
	b.mu.Unlock()
	b.sendPlain(c, chatID, text)
}

// conversation returns the chat's conversation, creating it if needed.
func (b *Bot) conversation(link channels.Link) (string, error) {
	if link.ConversationID != "" {
		if _, err := b.o.Store.GetConversation(b.ctx, link.UserID, link.ConversationID); err == nil {
			return link.ConversationID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	return b.newConversation(link)
}

func (b *Bot) newConversation(link channels.Link) (string, error) {
	conv, err := b.o.Store.CreateConversation(b.ctx, link.UserID, ConversationTitle, 0)
	if err != nil {
		return "", err
	}
	if err := b.o.Linker.SetConversation(b.ctx, Name, link.ExternalID, conv.ID); err != nil {
		return "", err
	}
	return conv.ID, nil
}

// maxQueued is how many messages may wait behind a running turn per user.
const maxQueued = 3

// startTurn runs the agent for one message. Turns are serialized per user:
// a message that arrives while a turn runs is queued (up to maxQueued) and
// runs when the current turn ends.
func (b *Bot) startTurn(c *Client, link channels.Link, chatID int64, text string) {
	w := b.watcher(link.UserID)
	if w == nil {
		return // shutting down
	}
	w.mu.Lock()
	if w.cur != nil || w.launching {
		if len(w.queue) >= maxQueued {
			w.mu.Unlock()
			b.sendPlain(c, chatID, "Still working on your earlier messages. Send this one again when I have answered.")
			return
		}
		w.queue = append(w.queue, queuedMsg{chatID: chatID, text: text})
		ack := !w.curDone
		w.mu.Unlock()
		b.log.Info("telegram: message queued", "chat_id", chatID, "user_id", link.UserID)
		if ack {
			b.sendPlain(c, chatID, "Got it. I will answer this after the current reply.")
		}
		return
	}
	w.launching = true
	w.mu.Unlock()
	b.launch(c, w, queuedMsg{chatID: chatID, text: text})
}

// launch starts a turn for m; w.launching must have been set by the caller.
// If m cannot run, the next queued message is tried.
func (b *Bot) launch(c *Client, w *watcher, m queuedMsg) {
	for {
		if b.tryLaunch(c, w, m) {
			return
		}
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.launching = false
			w.mu.Unlock()
			return
		}
		m = w.queue[0]
		w.queue = w.queue[1:]
		w.mu.Unlock()
	}
}

func (b *Bot) tryLaunch(c *Client, w *watcher, m queuedMsg) bool {
	// Re-check the link: a queued message must not run after an unlink.
	link, err := b.o.Linker.Lookup(b.ctx, Name, strconv.FormatInt(m.chatID, 10))
	if err != nil || link.UserID != w.userID {
		b.log.Info("telegram: dropping message, chat no longer linked", "chat_id", m.chatID)
		return false
	}
	convID, err := b.conversation(link)
	if err != nil {
		b.log.Error("telegram: conversation", "chat_id", m.chatID, "err", err)
		b.sendPlain(c, m.chatID, "Could not open your conversation.")
		return false
	}
	ctx, cancel := context.WithCancel(b.ctx)
	t := &turn{chatID: m.chatID, convID: convID, cancel: cancel}
	w.mu.Lock()
	w.cur, w.curDone, w.launching = t, false, false
	w.mu.Unlock()
	b.log.Info("telegram: turn started", "chat_id", m.chatID, "user_id", w.userID, "conversation_id", convID)
	_ = c.SendChatAction(b.ctx, m.chatID, "typing")
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer cancel()
		err := b.o.Runner.Run(ctx, w.userID, convID, m.text)
		w.done <- turnResult{t: t, err: err} // buffered; the watcher drains it
	}()
	return true
}

func (b *Bot) listApprovals(c *Client, userID, chatID int64) {
	pending, err := b.o.Approvals.ListPending(b.ctx, userID)
	if err != nil {
		b.log.Error("telegram: list approvals", "chat_id", chatID, "err", err)
		b.sendPlain(c, chatID, "Could not list approvals.")
		return
	}
	if len(pending) == 0 {
		b.sendPlain(c, chatID, "No approvals are waiting for you.")
		return
	}
	for _, a := range pending {
		b.postApproval(c, a.ID, a.Tool, string(a.Risk), a.Args, []int64{chatID})
	}
}

// --- approvals ---

// approvalPost tracks the messages showing one approval's keyboard.
type approvalPost struct {
	text     string
	entities []MessageEntity
	refs     []msgRef
}

type msgRef struct{ chatID, msgID int64 }

const callbackPrefix = "a:"

func approvalText(tool, risk string, args json.RawMessage) (string, []MessageEntity) {
	head := fmt.Sprintf("Approval needed: %s (%s risk)", tool, risk)
	a := compactArgs(args, 1500)
	text := head + "\n\n" + a
	return text, []MessageEntity{
		{Type: "bold", Offset: 0, Length: u16len(head)},
		{Type: "pre", Offset: u16len(head + "\n\n"), Length: u16len(a), Language: "json"},
	}
}

func compactArgs(raw json.RawMessage, max int) string {
	var buf strings.Builder
	var v any
	s := string(raw)
	if json.Unmarshal(raw, &v) == nil {
		if b, err := json.MarshalIndent(v, "", "  "); err == nil {
			s = string(b)
		}
	}
	if strings.TrimSpace(s) == "" {
		s = "{}"
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	buf.WriteString(s)
	return buf.String()
}

// postApproval sends an approval with Approve/Deny buttons to chats and
// remembers the messages so they can be updated when it is resolved.
func (b *Bot) postApproval(c *Client, id, tool, risk string, args json.RawMessage, chats []int64) {
	text, ents := approvalText(tool, risk, args)
	kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
		{Text: "Approve", CallbackData: callbackPrefix + id + ":y"},
		{Text: "Deny", CallbackData: callbackPrefix + id + ":n"},
	}}}
	for _, chatID := range chats {
		m, err := c.SendMessage(b.ctx, SendMessageParams{ChatID: chatID, Text: text, Entities: ents, ReplyMarkup: kb})
		if err != nil {
			b.log.Warn("telegram: send approval", "chat_id", chatID, "approval_id", id, "err", err)
			continue
		}
		b.mu.Lock()
		p := b.approvals[id]
		if p == nil {
			p = &approvalPost{text: text, entities: ents}
			b.approvals[id] = p
		}
		p.refs = append(p.refs, msgRef{chatID, m.MessageID})
		b.mu.Unlock()
		b.log.Info("telegram: approval posted", "chat_id", chatID, "approval_id", id)
	}
}

func outcomeText(decision string) string {
	switch decision {
	case string(approval.Approve):
		return "Approved by you"
	case string(approval.Deny):
		return "Denied"
	case approval.DecisionExpired:
		return "Expired (denied)"
	case approval.DecisionCanceled:
		return "Canceled"
	}
	return decision
}

// approvalResolved replaces the keyboard of every message showing the
// approval with its outcome. It runs at most once per approval.
func (b *Bot) approvalResolved(c *Client, id, decision string) {
	b.mu.Lock()
	p := b.approvals[id]
	delete(b.approvals, id)
	b.mu.Unlock()
	if p == nil || c == nil {
		return
	}
	text := p.text + "\n\n" + outcomeText(decision)
	ents := append(append([]MessageEntity(nil), p.entities...),
		MessageEntity{Type: "italic", Offset: u16len(p.text + "\n\n"), Length: u16len(outcomeText(decision))})
	for _, r := range p.refs {
		if err := c.EditMessageText(b.ctx, EditMessageTextParams{ChatID: r.chatID, MessageID: r.msgID, Text: text, Entities: ents}); err != nil {
			b.log.Warn("telegram: update approval message", "chat_id", r.chatID, "approval_id", id, "err", err)
		}
	}
}

func (b *Bot) handleCallback(c *Client, q *CallbackQuery) {
	answer := func(text string) {
		if err := c.AnswerCallbackQuery(b.ctx, q.ID, text); err != nil {
			b.log.Warn("telegram: answer callback", "err", err)
		}
	}
	m := q.Message
	if m == nil || m.Chat.Type != "private" || q.From.ID != m.Chat.ID {
		answer("")
		return
	}
	link, err := b.o.Linker.Lookup(b.ctx, Name, strconv.FormatInt(m.Chat.ID, 10))
	if err != nil {
		b.log.Info("telegram: callback from unlinked chat", "chat_id", m.Chat.ID)
		answer("This chat is not linked.")
		return
	}
	if u, err := b.o.Store.GetUser(b.ctx, link.UserID); err != nil || u.Disabled {
		answer("This Bosun account is not available.")
		return
	}
	rest, ok := strings.CutPrefix(q.Data, callbackPrefix)
	id, verdict, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || id == "" || (verdict != "y" && verdict != "n") {
		answer("Unknown action.")
		return
	}
	d := approval.Deny
	if verdict == "y" {
		d = approval.Approve
	}
	err = b.o.Approvals.Resolve(b.ctx, id, link.UserID, d)
	b.log.Info("telegram: approval answered", "chat_id", m.Chat.ID, "approval_id", id, "decision", string(d), "ok", err == nil)
	switch {
	case err == nil:
		if d == approval.Approve {
			answer("Approved")
		} else {
			answer("Denied")
		}
		b.approvalResolved(c, id, string(d))
		return
	case errors.Is(err, approval.ErrAlreadyResolved):
		answer("Already answered.")
	case errors.Is(err, approval.ErrExpired):
		answer("This approval expired.")
	case errors.Is(err, approval.ErrNotFound), errors.Is(err, approval.ErrForbidden):
		answer("Unknown approval.")
	default:
		b.log.Error("telegram: resolve approval", "approval_id", id, "err", err)
		answer("Something went wrong.")
		return
	}
	// The approval is settled elsewhere; drop the stale keyboard.
	b.mu.Lock()
	_, tracked := b.approvals[id]
	b.mu.Unlock()
	if !tracked {
		_ = c.EditMessageText(b.ctx, EditMessageTextParams{ChatID: m.Chat.ID, MessageID: m.MessageID, Text: m.Text, Entities: m.Entities})
	}
}

// --- sending ---

func (b *Bot) sendPlain(c *Client, chatID int64, text string) {
	if _, err := c.SendMessage(b.ctx, SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		b.log.Warn("telegram: send message", "chat_id", chatID, "err", err)
	}
}

// sendRendered sends rendered text, falling back to plain text if
// Telegram rejects the entities.
func (b *Bot) sendRendered(c *Client, chatID int64, text string, ents []MessageEntity) (int64, error) {
	m, err := c.SendMessage(b.ctx, SendMessageParams{ChatID: chatID, Text: text, Entities: ents})
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == http.StatusBadRequest && len(ents) > 0 {
		m, err = c.SendMessage(b.ctx, SendMessageParams{ChatID: chatID, Text: text})
	}
	if err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

func (b *Bot) editRendered(c *Client, chatID, msgID int64, text string, ents []MessageEntity) error {
	err := c.EditMessageText(b.ctx, EditMessageTextParams{ChatID: chatID, MessageID: msgID, Text: text, Entities: ents})
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == http.StatusBadRequest && len(ents) > 0 {
		err = c.EditMessageText(b.ctx, EditMessageTextParams{ChatID: chatID, MessageID: msgID, Text: text})
	}
	return err
}

// sendLong sends markdown text, split across messages if needed, with an
// optional bold header on the first one.
func (b *Bot) sendLong(c *Client, chatID int64, header, md string) {
	for i, part := range splitText(md, MaxMessageLen-u16len(header)-2) {
		text, ents := Render(part)
		if i == 0 {
			text, ents = withHeader(header, text, ents)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if _, err := b.sendRendered(c, chatID, text, ents); err != nil {
			b.log.Warn("telegram: send message", "chat_id", chatID, "err", err)
			return
		}
	}
}

// chatsOf returns the user's linked Telegram chat IDs.
func (b *Bot) chatsOf(userID int64) []int64 {
	links, err := b.o.Linker.Links(b.ctx, userID, Name)
	if err != nil {
		b.log.Error("telegram: list user links", "user_id", userID, "err", err)
		return nil
	}
	out := make([]int64, 0, len(links))
	for _, l := range links {
		if id, err := strconv.ParseInt(l.ExternalID, 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// deliverRoutine posts a finished routine run's reply to the user's chats.
func (b *Bot) deliverRoutine(c *Client, userID int64, d events.RoutineFinishedData) {
	chats := b.chatsOf(userID)
	if len(chats) == 0 {
		return
	}
	name := fmt.Sprintf("#%d", d.RoutineID)
	if r, err := b.o.Store.GetRoutine(b.ctx, userID, d.RoutineID); err == nil {
		name = r.Name
	}
	run, err := b.o.Store.GetRoutineRun(b.ctx, userID, d.RunID)
	if err != nil {
		b.log.Warn("telegram: routine run lookup", "user_id", userID, "run_id", d.RunID, "err", err)
		return
	}
	var header, body string
	switch run.Status {
	case "done":
		header, body = "Routine "+name, run.Reply
		if strings.TrimSpace(body) == "" {
			body = "(no reply)"
		}
	case "error":
		header, body = "Routine "+name+" failed", run.Error
	default:
		header, body = "Routine "+name+" was "+run.Status, ""
	}
	for _, chatID := range chats {
		b.sendLong(c, chatID, header, body)
		b.log.Info("telegram: routine result delivered", "chat_id", chatID, "routine_id", d.RoutineID, "run_id", d.RunID)
	}
}
