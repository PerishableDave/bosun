package telegram

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/dperedo/bosun/internal/api/events"
)

// A watcher follows one linked user's event bus: it renders the user's
// Telegram turns, posts approvals (from any client or routine) with
// Approve/Deny buttons, and delivers routine results. All rendering happens
// on the watcher's goroutine, so per-turn state needs no locking.
type watcher struct {
	userID int64
	sub    *events.Subscription
	done   chan turnResult // buffered: at most one turn runs at a time
	stop   chan struct{}
	once   sync.Once

	mu        sync.Mutex
	cur       *turn
	curDone   bool        // cur has seen its done/error event
	launching bool        // a turn is being started
	queue     []queuedMsg // messages waiting for the current turn
}

// queuedMsg is a chat message waiting to run.
type queuedMsg struct {
	chatID int64
	text   string
}

type turnResult struct {
	t   *turn
	err error
}

// turn is one agent run started from a chat.
type turn struct {
	chatID int64
	convID string
	cancel context.CancelFunc

	// Rendering state (watcher goroutine only). text is the assistant text
	// of the current block (text between tool calls/approvals); the current
	// Telegram message shows text[segStart:].
	text     string
	segStart int
	msgID    int64
	shown    string
	lastEdit time.Time
	sentAny  bool
	terminal bool
}

func (t *turn) resetBlock() { t.text, t.segStart, t.msgID, t.shown = "", 0, 0, "" }

func (w *watcher) current() *turn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur
}

func (w *watcher) markDone() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.curDone = true
}

// end clears t and returns the next queued message to launch, if any (the
// caller must launch it: launching is set).
func (w *watcher) end(t *turn) (queuedMsg, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == t {
		w.cur = nil
	}
	if w.cur != nil || w.launching || len(w.queue) == 0 {
		return queuedMsg{}, false
	}
	m := w.queue[0]
	w.queue = w.queue[1:]
	w.launching = true
	return m, true
}

// close stops the watcher and cancels its turn, if any.
func (w *watcher) close() {
	w.once.Do(func() {
		close(w.stop)
		w.sub.Close()
		if t := w.current(); t != nil {
			t.cancel()
		}
	})
}

// watcher returns the user's watcher, subscribing to their events first if
// needed. It returns nil once the bot is closed.
func (b *Bot) watcher(userID int64) *watcher {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.ctx == nil {
		return nil
	}
	if w := b.watchers[userID]; w != nil {
		return w
	}
	w := &watcher{userID: userID, sub: b.o.Bus.Subscribe(userID, eventBuffer),
		done: make(chan turnResult, 1), stop: make(chan struct{})}
	b.watchers[userID] = w
	b.wg.Add(1)
	go b.watch(w)
	return w
}

// linksChanged starts or stops following a user when their Telegram links
// change (Linker.OnChange).
func (b *Bot) linksChanged(channel string, userID int64) {
	if channel != Name {
		return
	}
	if len(b.chatsOf(userID)) > 0 {
		b.watcher(userID)
		return
	}
	b.mu.Lock()
	w := b.watchers[userID]
	delete(b.watchers, userID)
	b.mu.Unlock()
	if w != nil {
		w.close()
	}
}

func (b *Bot) watch(w *watcher) {
	defer b.wg.Done()
	tick := min(max(b.o.EditInterval/4, 10*time.Millisecond), 250*time.Millisecond)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-b.ctx.Done():
			return
		case ev, ok := <-w.sub.C:
			if !ok {
				return
			}
			b.handleEvent(w, ev)
		case r := <-w.done:
			// Everything the turn published is already buffered; render it
			// before finishing the turn.
			for drained := false; !drained; {
				select {
				case ev, ok := <-w.sub.C:
					if !ok {
						drained = true
						break
					}
					b.handleEvent(w, ev)
				default:
					drained = true
				}
			}
			b.finishTurn(w, r)
		case <-ticker.C:
			if t := w.current(); t != nil && !t.terminal {
				if c := b.api(); c != nil {
					b.flush(c, t, false)
				}
			}
		}
	}
}

func (b *Bot) handleEvent(w *watcher, ev events.Event) {
	c := b.api()
	if c == nil {
		return
	}
	t := w.current()
	inTurn := t != nil && !t.terminal && events.ConversationOf(ev) == t.convID
	switch d := ev.Data.(type) {
	case events.TextData:
		if inTurn {
			t.text += d.Delta
		}
	case events.ToolCallData:
		if inTurn {
			b.flush(c, t, true)
			t.resetBlock()
			b.sendItalic(c, t.chatID, "⚙ "+d.Name)
			t.sentAny = true
		}
	case events.ApprovalRequiredData:
		if inTurn {
			b.flush(c, t, true)
			t.resetBlock()
			t.sentAny = true
		}
		// Approvals from anywhere (this chat, the web UI, a routine) go to
		// every linked chat, so they can be answered from the phone.
		b.postApproval(c, d.ApprovalID, d.Tool, d.Risk, d.Args, b.chatsOf(w.userID))
	case events.ApprovalResolvedData:
		b.approvalResolved(c, d.ApprovalID, d.Decision)
	case events.DoneData:
		if inTurn {
			t.terminal = true
			w.markDone()
			b.flush(c, t, true)
			if !t.sentAny {
				b.sendPlain(c, t.chatID, "(no reply)")
			}
		}
	case events.ErrorData:
		if inTurn {
			t.terminal = true
			w.markDone()
			b.flush(c, t, true)
			b.sendPlain(c, t.chatID, "Error: "+d.Message)
		}
	case events.RoutineFinishedData:
		b.deliverRoutine(c, w.userID, d)
	}
}

func (b *Bot) finishTurn(w *watcher, r turnResult) {
	t := r.t
	if c := b.api(); c != nil && !t.terminal {
		// The terminal event was dropped or never published.
		t.terminal = true
		b.flush(c, t, true)
		if r.err != nil {
			b.sendPlain(c, t.chatID, "Error: the turn failed.")
		} else if !t.sentAny {
			b.sendPlain(c, t.chatID, "(no reply)")
		}
	}
	next, ok := w.end(t)
	b.log.Info("telegram: turn finished", "chat_id", t.chatID, "user_id", w.userID, "conversation_id", t.convID, "ok", r.err == nil)
	if ok {
		b.launch(b.api(), w, next) // the client is set before any turn can start
	}
}

// flush brings the chat up to date with the turn's text. Streaming updates
// edit one message at most once per EditInterval; a final flush waits out
// the interval instead of skipping. Text beyond one message's limit
// continues in a new message.
func (b *Bot) flush(c *Client, t *turn, final bool) {
	seg := t.text[t.segStart:]
	for u16len(seg) > MaxMessageLen {
		cut := cutPoint(seg, MaxMessageLen)
		if !b.show(c, t, seg[:cut], true) {
			return
		}
		t.segStart += cut
		t.msgID, t.shown = 0, ""
		seg = t.text[t.segStart:]
	}
	if strings.TrimSpace(seg) == "" || seg == t.shown {
		return
	}
	b.show(c, t, seg, final)
}

// show displays seg in the turn's current message, sending it if there is
// none yet. It reports whether the chat now shows seg.
func (b *Bot) show(c *Client, t *turn, seg string, final bool) bool {
	text, ents := Render(seg)
	if t.msgID == 0 {
		id, err := b.sendRendered(c, t.chatID, text, ents)
		if err != nil {
			b.log.Warn("telegram: send reply", "chat_id", t.chatID, "err", err)
			return false
		}
		t.msgID, t.shown, t.lastEdit, t.sentAny = id, seg, time.Now(), true
		return true
	}
	if seg == t.shown {
		return true
	}
	if wait := b.o.EditInterval - time.Since(t.lastEdit); wait > 0 {
		if !final || !b.sleep(wait) {
			return false
		}
	}
	if err := b.editRendered(c, t.chatID, t.msgID, text, ents); err != nil {
		b.log.Warn("telegram: edit reply", "chat_id", t.chatID, "err", err)
		return false
	}
	t.shown, t.lastEdit = seg, time.Now()
	return true
}

func (b *Bot) sendItalic(c *Client, chatID int64, text string) {
	ents := []MessageEntity{{Type: "italic", Offset: 0, Length: u16len(text)}}
	if _, err := c.SendMessage(b.ctx, SendMessageParams{ChatID: chatID, Text: text, Entities: ents}); err != nil {
		b.log.Warn("telegram: send message", "chat_id", chatID, "err", err)
	}
}
