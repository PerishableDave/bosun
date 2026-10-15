package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is the public Bot API endpoint. A self-hosted Bot API server
// (github.com/tdlib/telegram-bot-api) can be used instead via
// channels.telegram.api_url.
const DefaultAPIURL = "https://api.telegram.org"

// Bot API types (only the fields Bosun uses).
type (
	// User is a Telegram user or bot.
	User struct {
		ID        int64  `json:"id"`
		IsBot     bool   `json:"is_bot"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name,omitempty"`
		Username  string `json:"username,omitempty"`
	}
	// Chat is a Telegram chat. Type is "private", "group", "supergroup" or
	// "channel".
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	// Message is a Telegram message.
	Message struct {
		MessageID int64           `json:"message_id"`
		From      *User           `json:"from,omitempty"`
		Chat      Chat            `json:"chat"`
		Date      int64           `json:"date"`
		Text      string          `json:"text,omitempty"`
		Entities  []MessageEntity `json:"entities,omitempty"`
	}
	// CallbackQuery is an inline keyboard button press.
	CallbackQuery struct {
		ID      string   `json:"id"`
		From    User     `json:"from"`
		Message *Message `json:"message,omitempty"`
		Data    string   `json:"data,omitempty"`
	}
	// Update is one getUpdates result.
	Update struct {
		UpdateID      int64          `json:"update_id"`
		Message       *Message       `json:"message,omitempty"`
		CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	}
	// MessageEntity marks up a span of text. Offset and Length are in
	// UTF-16 code units.
	MessageEntity struct {
		Type     string `json:"type"` // "italic", "bold", "code", "pre"
		Offset   int    `json:"offset"`
		Length   int    `json:"length"`
		Language string `json:"language,omitempty"`
	}
	// InlineKeyboardButton is a callback button.
	InlineKeyboardButton struct {
		Text         string `json:"text"`
		CallbackData string `json:"callback_data"`
	}
	// InlineKeyboardMarkup is a keyboard attached to a message.
	InlineKeyboardMarkup struct {
		InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
	}
	linkPreviewOptions struct {
		IsDisabled bool `json:"is_disabled"`
	}
)

// SendMessageParams are sendMessage's parameters. Text is sent as plain
// text (no parse_mode) with explicit entities, so nothing needs escaping.
type SendMessageParams struct {
	ChatID             int64                 `json:"chat_id"`
	Text               string                `json:"text"`
	Entities           []MessageEntity       `json:"entities,omitempty"`
	ReplyMarkup        *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	LinkPreviewOptions *linkPreviewOptions   `json:"link_preview_options,omitempty"`
}

// EditMessageTextParams are editMessageText's parameters. A nil ReplyMarkup
// removes the message's inline keyboard.
type EditMessageTextParams struct {
	ChatID             int64                 `json:"chat_id"`
	MessageID          int64                 `json:"message_id"`
	Text               string                `json:"text"`
	Entities           []MessageEntity       `json:"entities,omitempty"`
	ReplyMarkup        *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	LinkPreviewOptions *linkPreviewOptions   `json:"link_preview_options,omitempty"`
}

// APIError is an error response from the Bot API.
type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int // seconds, for 429
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// Unauthorized reports whether the token was rejected.
func (e *APIError) Unauthorized() bool {
	return e.Code == http.StatusUnauthorized || e.Code == http.StatusNotFound
}

// Client is a minimal stdlib Bot API client.
type Client struct {
	base  string // <api>/bot<token>/
	token string
	http  *http.Client
}

// NewClient returns a client for token against apiURL ("" = DefaultAPIURL).
func NewClient(apiURL, token string, hc *http.Client) *Client {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: strings.TrimRight(apiURL, "/") + "/bot" + token + "/", token: token, http: hc}
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// maxRetryAfter bounds how long a call waits out a 429 before giving up.
const maxRetryAfter = 5 * time.Second

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	err := c.callOnce(ctx, method, params, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == http.StatusTooManyRequests && ae.RetryAfter > 0 {
		wait := time.Duration(ae.RetryAfter) * time.Second
		if wait > maxRetryAfter {
			return err
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		return c.callOnce(ctx, method, params, out)
	}
	return err
}

func (c *Client) callOnce(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram %s: encode: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: %s", method, c.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(ctx, method, req, out)
}

// do sends a prepared Bot API request and decodes its result into out.
func (c *Client) do(ctx context.Context, method string, req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// *url.Error includes the URL, which contains the token.
		return fmt.Errorf("telegram %s: %s", method, c.redact(err.Error()))
	}
	defer resp.Body.Close()
	var ar apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&ar); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d: bad response: %w", method, resp.StatusCode, err)
	}
	if !ar.OK {
		ae := &APIError{Method: method, Code: ar.ErrorCode, Description: ar.Description}
		if ae.Code == 0 {
			ae.Code = resp.StatusCode
		}
		if ar.Parameters != nil {
			ae.RetryAfter = ar.Parameters.RetryAfter
		}
		return ae
	}
	if out != nil {
		if err := json.Unmarshal(ar.Result, out); err != nil {
			return fmt.Errorf("telegram %s: decode result: %w", method, err)
		}
	}
	return nil
}

func (c *Client) redact(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "<token>")
}

// GetMe returns the bot's own user.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var u User
	if err := c.call(ctx, "getMe", struct{}{}, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUpdates long-polls for updates after offset, waiting up to timeout.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	params := struct {
		Offset         int64    `json:"offset,omitempty"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{offset, int(timeout / time.Second), []string{"message", "callback_query"}}
	// The HTTP request must outlive the server-side long poll.
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	var ups []Update
	if err := c.callOnce(ctx, "getUpdates", params, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}

// SendMessage sends a message and returns it.
func (c *Client) SendMessage(ctx context.Context, p SendMessageParams) (*Message, error) {
	p.LinkPreviewOptions = &linkPreviewOptions{IsDisabled: true} // no third-party fetches for previews
	var m Message
	if err := c.call(ctx, "sendMessage", p, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// EditMessageText replaces a message's text (and keyboard). Editing to
// identical content is not an error.
func (c *Client) EditMessageText(ctx context.Context, p EditMessageTextParams) error {
	p.LinkPreviewOptions = &linkPreviewOptions{IsDisabled: true}
	err := c.call(ctx, "editMessageText", p, nil)
	var ae *APIError
	if errors.As(err, &ae) && strings.Contains(ae.Description, "message is not modified") {
		return nil
	}
	return err
}

// SendPhoto uploads a PNG or JPEG image to chatID with an optional caption
// (at most 1024 characters) and returns the message.
func (c *Client) SendPhoto(ctx context.Context, chatID int64, photo []byte, filename, caption string) (*Message, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	fw, err := mw.CreateFormFile("photo", filename)
	if err != nil {
		return nil, fmt.Errorf("telegram sendPhoto: %w", err)
	}
	if _, err := fw.Write(photo); err != nil {
		return nil, fmt.Errorf("telegram sendPhoto: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("telegram sendPhoto: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"sendPhoto", &buf)
	if err != nil {
		return nil, fmt.Errorf("telegram sendPhoto: %s", c.redact(err.Error()))
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var m Message
	if err := c.do(ctx, "sendPhoto", req, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// AnswerCallbackQuery acknowledges a button press, optionally showing text.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", struct {
		ID   string `json:"callback_query_id"`
		Text string `json:"text,omitempty"`
	}{id, text}, nil)
}

// SendChatAction shows a status such as "typing" in the chat.
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	return c.call(ctx, "sendChatAction", struct {
		ChatID int64  `json:"chat_id"`
		Action string `json:"action"`
	}{chatID, action}, nil)
}
