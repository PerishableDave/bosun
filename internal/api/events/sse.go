package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// SSEWriter writes Server-Sent Events to an HTTP response. It is not safe for
// concurrent use; serialise writes from a single goroutine.
type SSEWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// NewSSEWriter sets SSE headers, writes the 200 status, and flushes.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &SSEWriter{w: w, rc: http.NewResponseController(w)}
	if err := s.rc.Flush(); err != nil {
		return nil, fmt.Errorf("sse: response does not support flushing: %w", err)
	}
	return s, nil
}

// Send writes one event and flushes.
func (s *SSEWriter) Send(ev Event) error {
	if ev.Type == "" || strings.ContainsAny(ev.Type, "\r\n") {
		return errors.New("sse: invalid event type")
	}
	data := ev.Data
	if data == nil {
		data = struct{}{}
	}
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("sse: encode %s: %w", ev.Type, err)
	}
	// json.Marshal never emits raw newlines, so a single data: line suffices.
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
		return fmt.Errorf("sse: write: %w", err)
	}
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("sse: flush: %w", err)
	}
	return nil
}

// Comment writes an SSE comment line (used as a keep-alive) and flushes.
func (s *SSEWriter) Comment(text string) error {
	text = strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return fmt.Errorf("sse: write: %w", err)
	}
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("sse: flush: %w", err)
	}
	return nil
}
