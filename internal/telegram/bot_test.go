package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendMessageWithButton(t *testing.T) {
	var path string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	b := NewBot("TOKEN", srv.URL, nil)
	if err := b.SendMessage(context.Background(), 42, "alerte", &Button{Text: "Silence", Data: "silence"}); err != nil {
		t.Fatal(err)
	}
	if path != "/botTOKEN/sendMessage" {
		t.Fatalf("path = %q", path)
	}
	if got["chat_id"].(float64) != 42 || got["text"] != "alerte" {
		t.Fatalf("body = %v", got)
	}
	btn := got["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
	if btn["text"] != "Silence" || btn["callback_data"] != "silence" {
		t.Fatalf("button = %v", btn)
	}
}

func TestSendPhotoMultipart(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0x00, 0x01, 0xff, 0xd9}
	var photo []byte
	var caption, chat, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		caption, chat = r.FormValue("caption"), r.FormValue("chat_id")
		f, _, err := r.FormFile("photo")
		if err != nil {
			t.Error(err)
			return
		}
		photo, _ = io.ReadAll(f)
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	b := NewBot("TOKEN", srv.URL, nil)
	if err := b.SendPhoto(context.Background(), 42, jpeg, "mouvement"); err != nil {
		t.Fatal(err)
	}
	if path != "/botTOKEN/sendPhoto" || caption != "mouvement" || chat != "42" || string(photo) != string(jpeg) {
		t.Fatalf("path=%q caption=%q chat=%q photo=%v", path, caption, chat, photo)
	}
}

func TestGetUpdatesParsesMessageAndCallback(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[
{"update_id":10,"message":{"chat":{"id":1},"from":{"id":2,"first_name":"Guillaume"},"text":"/status"}},
{"update_id":11,"callback_query":{"id":"cb1","from":{"id":3,"first_name":"Julie"},"data":"silence","message":{"chat":{"id":4}}}}]}`)
	}))
	defer srv.Close()
	b := NewBot("TOKEN", srv.URL, nil)
	ups, err := b.GetUpdates(context.Background(), 10, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got["offset"].(float64) != 10 || got["timeout"].(float64) != 30 {
		t.Fatalf("body = %v", got)
	}
	want := []Update{
		{ID: 10, ChatID: 1, FromID: 2, FromName: "Guillaume", Text: "/status"},
		{ID: 11, ChatID: 4, FromID: 3, FromName: "Julie", Callback: "cb1", Data: "silence"},
	}
	if len(ups) != 2 || ups[0] != want[0] || ups[1] != want[1] {
		t.Fatalf("updates = %+v", ups)
	}
}

func TestAPIErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	defer srv.Close()
	b := NewBot("TOKEN", srv.URL, nil)
	err := b.AnswerCallback(context.Background(), "cb", "ok")
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("err = %v", err)
	}
}

func TestBotErrorsHideToken(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	b := NewBot("123:SECRET", url, nil)
	_, err := b.GetUpdates(context.Background(), 0, time.Second)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("transport err = %v", err)
	}
	if err := b.SendPhoto(context.Background(), 1, []byte("x"), ""); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("photo err = %v", err)
	}
	if err := b.SendVideo(context.Background(), 1, []byte("x"), "", 1, 1, time.Second); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("video err = %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"description":"bad"}`)
	}))
	defer srv.Close()
	b = NewBot("123:SECRET", srv.URL, nil)
	if err := b.SendMessage(context.Background(), 1, "x", nil); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("api err = %v", err)
	}
}

func TestSendKeyboard(t *testing.T) {
	var path string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	b := NewBot("TOKEN", srv.URL, nil)
	kb := Keyboard{{"🔒 Armer", "🌙 Armer (nuit)"}, {"ℹ️ État"}}
	if err := b.SendKeyboard(context.Background(), 42, "ok", kb); err != nil {
		t.Fatal(err)
	}
	if path != "/botTOKEN/sendMessage" || got["chat_id"].(float64) != 42 || got["text"] != "ok" {
		t.Fatalf("path=%q body=%v", path, got)
	}
	rm := got["reply_markup"].(map[string]any)
	if rm["is_persistent"] != true || rm["resize_keyboard"] != true {
		t.Fatalf("reply_markup = %v", rm)
	}
	rows := rm["keyboard"].([]any)
	if len(rows) != 2 || len(rows[0].([]any)) != 2 || len(rows[1].([]any)) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0].([]any)[1].(map[string]any)["text"] != "🌙 Armer (nuit)" || rows[1].([]any)[0].(map[string]any)["text"] != "ℹ️ État" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestSendButtons(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	b := NewBot("TOKEN", srv.URL, nil)
	err := b.SendButtons(context.Background(), 42, "sûr ?", []Button{{"Oui", "yes:1"}, {"Non", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	rows := got["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	if len(rows) != 1 || len(rows[0].([]any)) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	b0 := rows[0].([]any)[0].(map[string]any)
	b1 := rows[0].([]any)[1].(map[string]any)
	if b0["text"] != "Oui" || b0["callback_data"] != "yes:1" || b1["text"] != "Non" || b1["callback_data"] != "no" {
		t.Fatalf("buttons = %v %v", b0, b1)
	}
}

func TestSendVideoMultipart(t *testing.T) {
	mp4 := []byte("\x00\x00\x00\x18ftypisom-fake-mp4-bytes")
	var video []byte
	var path, filename string
	var fields map[string]string
	var length int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, length = r.URL.Path, r.ContentLength
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		fields = map[string]string{}
		for _, k := range []string{"chat_id", "caption", "width", "height", "duration", "supports_streaming"} {
			fields[k] = r.FormValue(k)
		}
		f, h, err := r.FormFile("video")
		if err != nil {
			t.Error(err)
			return
		}
		filename = h.Filename
		video, _ = io.ReadAll(f)
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	b := NewBot("TOKEN", srv.URL, nil)
	if err := b.SendVideo(context.Background(), 42, mp4, "🎥 14:03:09", 1280, 720, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if path != "/botTOKEN/sendVideo" || filename != "clip.mp4" || string(video) != string(mp4) {
		t.Fatalf("path=%q filename=%q video=%q", path, filename, video)
	}
	want := map[string]string{"chat_id": "42", "caption": "🎥 14:03:09", "width": "1280", "height": "720", "duration": "15", "supports_streaming": "true"}
	for k, v := range want {
		if fields[k] != v {
			t.Fatalf("%s = %q, want %q (fields %v)", k, fields[k], v, fields)
		}
	}
	if length <= 0 {
		t.Fatalf("Content-Length = %d, want a known length", length)
	}
}

// captureTransport answers every request with ok after reading its body
// twice: once as sent, once through GetBody.
type captureTransport struct {
	first, again []byte
	length       int64
	noGetBody    bool
}

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.first, _ = io.ReadAll(r.Body)
	c.length = r.ContentLength
	if r.GetBody == nil {
		c.noGetBody = true
	} else {
		b, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		c.again, _ = io.ReadAll(b)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: http.Header{}}, nil
}

func TestSendVideoBodyRewindable(t *testing.T) {
	tr := &captureTransport{}
	b := NewBot("TOKEN", "http://telegram.invalid", &http.Client{Transport: tr})
	mp4 := []byte("fake-mp4-content")
	if err := b.SendVideo(context.Background(), 42, mp4, "c", 1, 1, time.Second); err != nil {
		t.Fatal(err)
	}
	if tr.noGetBody {
		t.Fatal("no GetBody: the request cannot be retried")
	}
	if len(tr.first) == 0 || string(tr.first) != string(tr.again) || int64(len(tr.first)) != tr.length {
		t.Fatalf("first %d bytes, again %d bytes, Content-Length %d", len(tr.first), len(tr.again), tr.length)
	}
	if !strings.Contains(string(tr.again), "fake-mp4-content") {
		t.Fatalf("body = %q", tr.again)
	}
}
