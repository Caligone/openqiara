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
	"net/url"
	"strconv"
	"time"
)

const (
	defaultBaseURL = "https://api.telegram.org"
	// callTimeout bounds every call except GetUpdates, whose deadline is
	// the long-poll timeout plus 10 s.
	callTimeout = 30 * time.Second
	// videoTimeout bounds SendVideo: a clip of a few MB on a slow uplink.
	videoTimeout = 120 * time.Second
)

// Bot is a minimal Telegram Bot API client. The token is a secret and never
// appears in returned errors.
type Bot struct {
	token   string
	baseURL string
	client  *http.Client
}

// NewBot builds a client. An empty baseURL means the public Telegram API. A nil
// client gets a plain http.Client without a global Timeout: deadlines are set
// per request through the context (30 s, or timeout+10 s for GetUpdates).
// A custom client must not impose a shorter Timeout than that.
func NewBot(token, baseURL string, client *http.Client) *Bot {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if client == nil {
		client = &http.Client{}
	}
	return &Bot{token: token, baseURL: baseURL, client: client}
}

// Button is a single inline keyboard button.
type Button struct{ Text, Data string }

// Update is a flattened incoming message or button press.
type Update struct {
	ID       int64
	ChatID   int64  // chat of the message, or of the message the button sits on
	FromID   int64  // sender of the message or of the press
	FromName string // first name of the sender, "" if absent
	Text     string // message text, "" for a press
	Callback string // callback query id, "" for a message
	Data     string // button data, "" for a message
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// call performs the request and returns the raw result. Errors never contain
// the request URL.
func (b *Bot) call(ctx context.Context, method, contentType string, body io.Reader, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/bot"+b.token+"/"+method, body)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: invalid request", method)
	}
	req.Header.Set("Content-Type", contentType)
	if s, ok := body.(*sizedReader); ok {
		req.ContentLength = s.size // sent with a Content-Length, not chunked
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(s.open()), nil }
	}
	resp, err := b.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	var r apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("telegram %s: HTTP %d, invalid response", method, resp.StatusCode)
	}
	if !r.OK {
		return nil, fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	return r.Result, nil
}

func (b *Bot) callJSON(ctx context.Context, method string, payload any, timeout time.Duration) (json.RawMessage, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: %w", method, err)
	}
	return b.call(ctx, method, "application/json", bytes.NewReader(data), timeout)
}

// SendMessage sends text, with an optional inline button.
func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string, button *Button) error {
	p := map[string]any{"chat_id": chatID, "text": text}
	if button != nil {
		p["reply_markup"] = map[string]any{
			"inline_keyboard": [][]map[string]string{{{"text": button.Text, "callback_data": button.Data}}},
		}
	}
	_, err := b.callJSON(ctx, "sendMessage", p, callTimeout)
	return err
}

// Keyboard is rows of button texts, sent as a persistent, resized reply
// keyboard.
type Keyboard [][]string

// SendKeyboard sends text and installs a persistent reply keyboard.
func (b *Bot) SendKeyboard(ctx context.Context, chatID int64, text string, kb Keyboard) error {
	rows := make([][]map[string]string, 0, len(kb))
	for _, r := range kb {
		row := make([]map[string]string, 0, len(r))
		for _, t := range r {
			row = append(row, map[string]string{"text": t})
		}
		rows = append(rows, row)
	}
	p := map[string]any{"chat_id": chatID, "text": text, "reply_markup": map[string]any{
		"keyboard": rows, "is_persistent": true, "resize_keyboard": true,
	}}
	_, err := b.callJSON(ctx, "sendMessage", p, callTimeout)
	return err
}

// SendButtons sends text with an inline keyboard of one row.
func (b *Bot) SendButtons(ctx context.Context, chatID int64, text string, buttons []Button) error {
	row := make([]map[string]string, 0, len(buttons))
	for _, bt := range buttons {
		row = append(row, map[string]string{"text": bt.Text, "callback_data": bt.Data})
	}
	p := map[string]any{"chat_id": chatID, "text": text, "reply_markup": map[string]any{
		"inline_keyboard": [][]map[string]string{row},
	}}
	_, err := b.callJSON(ctx, "sendMessage", p, callTimeout)
	return err
}

// SendPhoto uploads a JPEG with a caption.
func (b *Bot) SendPhoto(ctx context.Context, chatID int64, jpeg []byte, caption string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	if caption != "" {
		if err := mw.WriteField("caption", caption); err != nil {
			return fmt.Errorf("telegram sendPhoto: %w", err)
		}
	}
	fw, err := mw.CreateFormFile("photo", "alerte.jpg")
	if err != nil {
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	if _, err := fw.Write(jpeg); err != nil {
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	_, err = b.call(ctx, "sendPhoto", mw.FormDataContentType(), &buf, callTimeout)
	return err
}

// sizedReader is a body of known length, made of parts read in turn, that
// can be read again from the start (for a retry) without copying them.
type sizedReader struct {
	io.Reader
	parts [][]byte
	size  int64
}

func newSizedReader(parts ...[]byte) *sizedReader {
	s := &sizedReader{parts: parts}
	for _, p := range parts {
		s.size += int64(len(p))
	}
	s.Reader = s.open()
	return s
}

// open returns a fresh reader over all the parts.
func (s *sizedReader) open() io.Reader {
	rs := make([]io.Reader, len(s.parts))
	for i, p := range s.parts {
		rs[i] = bytes.NewReader(p)
	}
	return io.MultiReader(rs...)
}

// SendVideo uploads an MP4 (multipart: chat_id, caption, video=clip.mp4,
// width, height, duration in whole seconds, supports_streaming=true).
// Its request timeout is videoTimeout (120 s), not the default 30 s.
// The MP4 is streamed from mp4, never copied.
func (b *Bot) SendVideo(ctx context.Context, chatID int64, mp4 []byte, caption string, width, height int, duration time.Duration) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := [][2]string{{"chat_id", strconv.FormatInt(chatID, 10)}}
	if caption != "" {
		fields = append(fields, [2]string{"caption", caption})
	}
	fields = append(fields,
		[2]string{"width", strconv.Itoa(width)},
		[2]string{"height", strconv.Itoa(height)},
		[2]string{"duration", strconv.FormatInt(int64((duration+time.Second/2)/time.Second), 10)},
		[2]string{"supports_streaming", "true"},
	)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return fmt.Errorf("telegram sendVideo: %w", err)
		}
	}
	if _, err := mw.CreateFormFile("video", "clip.mp4"); err != nil {
		return fmt.Errorf("telegram sendVideo: %w", err)
	}
	// The file part is last: its content goes between the headers written
	// so far and the closing boundary that Close appends.
	split := buf.Len()
	if err := mw.Close(); err != nil {
		return fmt.Errorf("telegram sendVideo: %w", err)
	}
	body := newSizedReader(buf.Bytes()[:split], mp4, buf.Bytes()[split:])
	_, err := b.call(ctx, "sendVideo", mw.FormDataContentType(), body, videoTimeout)
	return err
}

// GetUpdates long-polls for updates starting at offset. The request deadline
// is timeout + 10 s.
func (b *Bot) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	p := map[string]any{
		"offset":          offset,
		"timeout":         int64(timeout / time.Second),
		"allowed_updates": []string{"message", "callback_query"},
	}
	raw, err := b.callJSON(ctx, "getUpdates", p, timeout+10*time.Second)
	if err != nil {
		return nil, err
	}
	var in []struct {
		ID      int64 `json:"update_id"`
		Message *struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			From struct {
				ID        int64  `json:"id"`
				FirstName string `json:"first_name"`
			} `json:"from"`
			Text string `json:"text"`
		} `json:"message"`
		Callback *struct {
			ID   string `json:"id"`
			From struct {
				ID        int64  `json:"id"`
				FirstName string `json:"first_name"`
			} `json:"from"`
			Data    string `json:"data"`
			Message struct {
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"callback_query"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("telegram getUpdates: invalid result")
	}
	out := make([]Update, 0, len(in))
	for _, u := range in {
		switch {
		case u.Message != nil:
			out = append(out, Update{ID: u.ID, ChatID: u.Message.Chat.ID, FromID: u.Message.From.ID, FromName: u.Message.From.FirstName, Text: u.Message.Text})
		case u.Callback != nil:
			out = append(out, Update{ID: u.ID, ChatID: u.Callback.Message.Chat.ID, FromID: u.Callback.From.ID, FromName: u.Callback.From.FirstName, Callback: u.Callback.ID, Data: u.Callback.Data})
		default:
			out = append(out, Update{ID: u.ID})
		}
	}
	return out, nil
}

// AnswerCallback acknowledges a button press, optionally showing text.
func (b *Bot) AnswerCallback(ctx context.Context, callbackID, text string) error {
	p := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		p["text"] = text
	}
	_, err := b.callJSON(ctx, "answerCallbackQuery", p, callTimeout)
	return err
}
