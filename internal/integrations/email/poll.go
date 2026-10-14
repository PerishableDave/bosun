package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/integration"
)

// EventReceived is emitted once per new INBOX message.
const EventReceived = "email.received"

// maxEmitPerPoll bounds events per user per poll; when more arrive at once
// (e.g. after a long outage) only the newest are announced.
const maxEmitPerPoll = 20

// ReceivedPayload is the email.received event payload. Every field is
// untrusted (it comes from whoever sent the mail).
type ReceivedPayload struct {
	UID     uint32 `json:"uid"`
	Folder  string `json:"folder"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Snippet string `json:"snippet"`
	Date    string `json:"date"`
}

// folderState is the poller's high-water mark for one user's folder.
type folderState struct {
	UIDValidity uint32 `json:"uidvalidity"`
	LastUID     uint32 `json:"last_uid"`
}

// pollState persists last-seen UIDs (key "<userID>/<folder>") in
// <data_dir>/poll_state.json so restarts do not re-emit.
type pollState struct {
	mu   sync.Mutex
	path string
	m    map[string]folderState
}

func loadPollState(dir string) (*pollState, error) {
	s := &pollState{path: filepath.Join(dir, "poll_state.json"), m: map[string]folderState{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s.m); err != nil {
		s.m = map[string]folderState{}
		return s, err
	}
	return s, nil
}

func stateKey(userID int64, folder string) string {
	return strconv.FormatInt(userID, 10) + "/" + folder
}

func (s *pollState) get(userID int64, folder string) (folderState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[stateKey(userID, folder)]
	return v, ok
}

// set records and saves (atomically: temp file + rename).
func (s *pollState) set(userID int64, folder string, v folderState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[stateKey(userID, folder)] = v
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".poll_state-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// pollLoop waits until the daemon has wired the emitter and user lister,
// then polls immediately and every e.poll until Close.
func (e *Integration) pollLoop(ctx context.Context) {
	defer close(e.done)
	select {
	case <-ctx.Done():
		return
	case <-e.ready:
	}
	t := time.NewTicker(e.poll)
	defer t.Stop()
	for {
		e.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollOnce checks every listed user's INBOX once.
func (e *Integration) pollOnce(ctx context.Context) {
	e.mu.Lock()
	list, emit := e.listUsers, e.emit
	e.mu.Unlock()
	if list == nil || emit == nil || e.state == nil {
		return
	}
	users, err := list(ctx)
	if err != nil {
		e.log.Warn("email: list users for polling", "err", err)
		return
	}
	for _, uid := range users {
		if ctx.Err() != nil {
			return
		}
		err := e.pollUser(integration.WithUserID(ctx, uid), uid, emit)
		e.noteErr(uid, err)
	}
}

// noteErr logs a user's poll error only when it changes, so a broken
// account does not flood the log every interval.
func (e *Integration) noteErr(userID int64, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	e.mu.Lock()
	prev := e.lastErr[userID]
	e.lastErr[userID] = msg
	e.mu.Unlock()
	switch {
	case msg != "" && msg != prev:
		e.log.Warn("email: poll failed", "user_id", userID, "err", err)
	case msg == "" && prev != "":
		e.log.Info("email: poll recovered", "user_id", userID)
	}
}

// pollUser emits email.received for INBOX messages newer than the stored
// high-water mark. The first time a user (or a new UIDVALIDITY) is seen it
// only records the mark, so existing mail is not announced.
func (e *Integration) pollUser(ctx context.Context, userID int64, emit integration.EmitFunc) error {
	const folder = "INBOX"
	a, err := e.resolveAccount(ctx)
	if errors.Is(err, errNoAccount) {
		return nil // not linked (any more): nothing to poll
	}
	if err != nil {
		return err
	}
	c, err := e.openIMAP(ctx, a)
	if err != nil {
		return err
	}
	defer c.logout()
	st, err := c.selectFolder(folder, true)
	if err != nil {
		return err
	}
	prev, ok := e.state.get(userID, folder)
	if !ok || prev.UIDValidity != st.UIDValidity {
		mark := uint32(0)
		if st.UIDNext > 0 {
			mark = st.UIDNext - 1
		} else if all, err := c.uidSearch("ALL"); err == nil && len(all) > 0 {
			mark = newestUIDs(all, 1)[0]
		}
		return e.state.set(userID, folder, folderState{UIDValidity: st.UIDValidity, LastUID: mark})
	}
	if st.UIDNext > 0 && st.UIDNext <= prev.LastUID+1 {
		return nil // nothing new
	}
	found, err := c.uidSearch(fmt.Sprintf("UID %d:*", prev.LastUID+1))
	if err != nil {
		return err
	}
	var fresh []uint32
	for _, u := range found {
		if u > prev.LastUID { // "n:*" always matches the last message
			fresh = append(fresh, u)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i] < fresh[j] })
	high := fresh[len(fresh)-1]
	if len(fresh) > maxEmitPerPoll {
		e.log.Info("email: many new messages; announcing only the newest", "user_id", userID,
			"new", len(fresh), "announced", maxEmitPerPoll)
		fresh = fresh[len(fresh)-maxEmitPerPoll:]
	}
	fs, err := c.fetch(uidSet(fresh), true, summaryItems)
	if err != nil {
		return err
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].UID < fs[j].UID })
	for _, f := range fs {
		s := toSummary(f)
		payload, _ := json.Marshal(ReceivedPayload{UID: s.UID, Folder: folder, From: s.From, Subject: s.Subject,
			Snippet: s.Snippet, Date: s.Date})
		if err := emit(ctx, userID, EventReceived, payload); err != nil {
			e.log.Warn("email: emit event", "user_id", userID, "uid", f.UID, "err", err)
		}
		// Advance per message so a crash mid-batch does not re-emit.
		if err := e.state.set(userID, folder, folderState{UIDValidity: st.UIDValidity, LastUID: f.UID}); err != nil {
			return fmt.Errorf("save poll state: %w", err)
		}
	}
	return e.state.set(userID, folder, folderState{UIDValidity: st.UIDValidity, LastUID: high})
}
