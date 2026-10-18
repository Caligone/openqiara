package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/alarm"
	"github.com/caligone/openqiara/internal/clip"
)

type fakeCall struct {
	Method string
	Body   string
}

type fakeTelegram struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []fakeCall
	updates  []string // JSON objects returned by the next getUpdates
	msgFail  int      // sendMessage answers 500 this many times first
	msgSeen  int
	offsets  []int64       // offset of every getUpdates call
	failChat int64         // sendMessage to this chat always answers 500
	hang     chan struct{} // non-nil: every non-getUpdates call blocks until closed or client gone
	hangVid  chan struct{} // non-nil: sendVideo blocks until closed or client gone
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() {
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

func (f *fakeTelegram) handle(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	body, _ := io.ReadAll(r.Body)
	if method == "getUpdates" {
		var req struct{ Offset int64 }
		if err := json.Unmarshal(body, &req); err != nil {
			f.t.Errorf("decode request: %v", err)
		}
		f.mu.Lock()
		f.offsets = append(f.offsets, req.Offset)
		ups := f.updates
		f.updates = nil
		f.mu.Unlock()
		if len(ups) == 0 {
			select {
			case <-r.Context().Done():
			case <-time.After(20 * time.Millisecond):
				_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			}
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[`+strings.Join(ups, ",")+`]}`)
		return
	}
	f.mu.Lock()
	hang := f.hang
	if method == "sendVideo" && f.hangVid != nil {
		hang = f.hangVid
	}
	f.calls = append(f.calls, fakeCall{method, string(body)})
	fail := false
	if method == "sendMessage" {
		f.msgSeen++
		fail = f.msgSeen <= f.msgFail
		var req struct {
			ChatID int64 `json:"chat_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			f.t.Errorf("decode request: %v", err)
		}
		if f.failChat != 0 && req.ChatID == f.failChat {
			fail = true
		}
	}
	f.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
			return
		}
	}
	if fail {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "oops")
		return
	}
	_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
}

func (f *fakeTelegram) push(update string) {
	f.mu.Lock()
	f.updates = append(f.updates, update)
	f.mu.Unlock()
}

func (f *fakeTelegram) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// waitCalls waits until n calls other than getUpdates were recorded.
func (f *fakeTelegram) waitCalls(n int) []fakeCall {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := f.snapshot()
		if len(c) >= n {
			return c
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("timeout waiting for %d calls, got %v", n, c)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func methods(c []fakeCall) []string {
	var m []string
	for _, x := range c {
		m = append(m, x.Method)
	}
	return m
}

type fakeAlarm struct {
	mu        sync.Mutex
	n         int
	result    bool
	armCalls  [][2]bool // {night, ignoreOpen}
	armResult alarm.ArmResult
	inAlarm   func(night bool) []int
	snap      alarm.Snapshot
}

func (s *fakeAlarm) Silence() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return s.result
}

func (s *fakeAlarm) ArmRemote(night, ignoreOpen bool) alarm.ArmResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armCalls = append(s.armCalls, [2]bool{night, ignoreOpen})
	return s.armResult
}

func (s *fakeAlarm) InAlarm(night bool) []int {
	s.mu.Lock()
	fn := s.inAlarm
	s.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(night)
}

func (s *fakeAlarm) Snapshot() alarm.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s *fakeAlarm) arms() [][2]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]bool(nil), s.armCalls...)
}

func (s *fakeAlarm) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

const chatID = 42

var fixedNow = time.Date(2026, 10, 18, 14, 3, 9, 0, alertZone)

type notifierEnv struct {
	fake   *fakeTelegram
	sil    *fakeAlarm
	n      *Notifier
	now    *time.Time
	cancel context.CancelFunc // stops Run
	done   chan struct{}      // closed when Run returned
}

func startNotifier(t *testing.T, snap func(context.Context) ([]byte, error)) *notifierEnv {
	t.Helper()
	return startNotifierWith(t, snap, nil)
}

// startNotifierWith calls pre (if any) before Run starts, then waits for the
// startup drain to finish so later pushes are seen by the main poll loop.
func startNotifierWith(t *testing.T, snap func(context.Context) ([]byte, error), pre func(*fakeTelegram)) *notifierEnv {
	t.Helper()
	return startNotifierChats(t, snap, pre, chatID)
}

func startNotifierChats(t *testing.T, snap func(context.Context) ([]byte, error), pre func(*fakeTelegram), chats ...int64) *notifierEnv {
	t.Helper()
	return startNotifierOpts(t, snap, pre, nil, chats...)
}

// startNotifierOpts is startNotifierChats with a hook to adjust the options.
func startNotifierOpts(t *testing.T, snap func(context.Context) ([]byte, error), pre func(*fakeTelegram), mod func(*Options), chats ...int64) *notifierEnv {
	t.Helper()
	oldRetry, oldMax, oldPause := retryInitial, retryMax, pollPause
	retryInitial, retryMax, pollPause = time.Millisecond, 4*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { retryInitial, retryMax, pollPause = oldRetry, oldMax, oldPause })

	env := &notifierEnv{fake: newFakeTelegram(t), sil: &fakeAlarm{result: true, armResult: alarm.ArmDone}}
	if pre != nil {
		pre(env.fake)
	}
	now := fixedNow
	env.now = &now
	if snap == nil {
		snap = func(context.Context) ([]byte, error) { return []byte{0xff, 0xd8, 0xff, 0xd9}, nil }
	}
	var mu sync.Mutex
	o := Options{
		ChatIDs:  chats,
		Alarm:    env.sil,
		Snapshot: snap,
		Name:     func(id int) string { return fmt.Sprintf("Capteur %d", id) },
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return *env.now
		},
	}
	if mod != nil {
		mod(&o)
	}
	env.n = NewNotifier(NewBot("TOKEN", env.fake.srv.URL, nil), o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	env.cancel, env.done = cancel, done
	go func() { env.n.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for len(env.fake.getOffsets()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("poll loop did not start")
		}
		time.Sleep(time.Millisecond)
	}
	return env
}

func (f *fakeTelegram) getOffsets() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.offsets...)
}

func TestAlertSendsTextThenPhoto(t *testing.T) {
	env := startNotifier(t, nil)
	env.n.Alert(alarm.Alert{SensorID: 1})
	c := env.fake.waitCalls(2)
	if got := methods(c); got[0] != "sendMessage" || got[1] != "sendPhoto" {
		t.Fatalf("calls = %v", got)
	}
	var body struct {
		Text        string `json:"text"`
		ReplyMarkup struct {
			Keyboard [][]map[string]string `json:"inline_keyboard"`
		} `json:"reply_markup"`
	}
	if err := json.Unmarshal([]byte(c[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if want := "🚨 Alarme déclenchée — Capteur 1 — 14:03:09"; body.Text != want {
		t.Fatalf("text = %q, want %q", body.Text, want)
	}
	if b := body.ReplyMarkup.Keyboard[0][0]; b["text"] != "🔕 Couper la sirène" || b["callback_data"] != "silence" {
		t.Fatalf("button = %v", b)
	}
}

func TestAlertTexts(t *testing.T) {
	env := startNotifier(t, nil)
	env.n.Alert(alarm.Alert{SensorID: 2, Again: true})
	env.n.Alert(alarm.Alert{SensorID: 0})
	var texts []string
	for _, c := range env.fake.waitCalls(4) {
		if c.Method == "sendMessage" {
			texts = append(texts, messageText(c))
		}
	}
	if texts[0] != "🚨 Nouvelle détection — Capteur 2 — 14:03:09" {
		t.Fatalf("text0 = %q", texts[0])
	}
	if texts[1] != "🚨 Alarme déclenchée — la sirène — 14:03:09" {
		t.Fatalf("text1 = %q", texts[1])
	}
}

func TestTextSentWhenSnapshotFails(t *testing.T) {
	env := startNotifier(t, func(context.Context) ([]byte, error) { return nil, errors.New("no camera") })
	env.n.Alert(alarm.Alert{SensorID: 1})
	env.fake.waitCalls(1)
	// A second alert (other sensor) proves the first one is fully processed.
	env.n.Alert(alarm.Alert{SensorID: 2})
	c := env.fake.waitCalls(2)
	for _, x := range c {
		if x.Method == "sendPhoto" {
			t.Fatalf("unexpected photo: %v", methods(c))
		}
	}
}

func countMethod(c []fakeCall, m string) int {
	n := 0
	for _, x := range c {
		if x.Method == m {
			n++
		}
	}
	return n
}

// waitMessage waits until a sendMessage containing substr was recorded and
// returns all calls so far. The sender is sequential, so every earlier alert
// has been fully handled by then.
func (f *fakeTelegram) waitMessage(substr string) []fakeCall {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := f.snapshot()
		for _, x := range c {
			if x.Method == "sendMessage" && strings.Contains(x.Body, substr) {
				return c
			}
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("timeout waiting for message %q, got %v", substr, methods(c))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func countMessages(c []fakeCall, substr string) int {
	n := 0
	for _, x := range c {
		if x.Method == "sendMessage" && strings.Contains(x.Body, substr) {
			n++
		}
	}
	return n
}

func TestRateLimitPerSensor(t *testing.T) {
	env := startNotifier(t, nil)
	env.n.Alert(alarm.Alert{SensorID: 2})
	env.n.Alert(alarm.Alert{SensorID: 2, Again: true})
	env.n.Alert(alarm.Alert{SensorID: 3})
	env.n.Alert(alarm.Alert{SensorID: 4}) // sentinel: queued after the others
	c := env.fake.waitMessage("Capteur 4")
	if n := countMessages(c, "Capteur 2"); n != 1 {
		t.Fatalf("sensor 2 messages = %d, want 1 (%v)", n, methods(c))
	}
	if n := countMessages(c, "Capteur 3"); n != 1 {
		t.Fatalf("sensor 3 messages = %d, want 1", n)
	}
	*env.now = fixedNow.Add(61 * time.Second)
	env.n.Alert(alarm.Alert{SensorID: 2, Again: true})
	env.n.Alert(alarm.Alert{SensorID: 5}) // sentinel
	c = env.fake.waitMessage("Capteur 5")
	if n := countMessages(c, "Capteur 2"); n != 2 {
		t.Fatalf("sensor 2 messages = %d, want 2 (%v)", n, methods(c))
	}
}

func TestAlertNeverBlocks(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.mu.Lock()
	env.fake.hang = make(chan struct{})
	env.fake.mu.Unlock()
	start := time.Now()
	for i := 1; i <= 100; i++ {
		env.n.Alert(alarm.Alert{SensorID: i})
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("100 Alert calls took %v", d)
	}
}

func TestTextRetried(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.mu.Lock()
	env.fake.msgFail = 2
	env.fake.mu.Unlock()
	env.n.Alert(alarm.Alert{SensorID: 1})
	c := env.fake.waitCalls(4) // 3 messages + photo
	if got := methods(c); countMethod(c, "sendMessage") != 3 || got[3] != "sendPhoto" {
		t.Fatalf("calls = %v", got)
	}
}

func pressUpdate(id int64, chat, from int64, data string) string {
	return fmt.Sprintf(`{"update_id":%d,"callback_query":{"id":"cb%d","from":{"id":%d},"data":%q,"message":{"chat":{"id":%d}}}}`, id, id, from, data, chat)
}

func messageText(c fakeCall) string {
	var b struct{ Text string }
	if err := json.Unmarshal([]byte(c.Body), &b); err != nil {
		return "invalid JSON body: " + err.Error()
	}
	return b.Text
}

func TestSilenceButton(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(pressUpdate(1, chatID, chatID, "silence"))
	c := env.fake.waitCalls(2)
	if c[0].Method != "answerCallbackQuery" || !strings.Contains(c[0].Body, "Sirène coupée") {
		t.Fatalf("call0 = %v", c[0])
	}
	if c[1].Method != "sendMessage" || messageText(c[1]) != "🔕 Sirène coupée. L'alarme reste armée." {
		t.Fatalf("call1 = %v", c[1])
	}
	if env.sil.count() != 1 {
		t.Fatalf("Silence calls = %d", env.sil.count())
	}
}

func TestSilenceButtonNothingToStop(t *testing.T) {
	env := startNotifier(t, nil)
	env.sil.result = false
	env.fake.push(pressUpdate(1, chatID, chatID, "silence"))
	c := env.fake.waitCalls(2)
	if c[0].Method != "answerCallbackQuery" || !strings.Contains(c[0].Body, "Rien à couper") {
		t.Fatalf("call0 = %v", c[0])
	}
	if messageText(c[1]) != "ℹ️ Rien à couper : l'alarme n'est pas déclenchée." {
		t.Fatalf("call1 = %v", c[1])
	}
}

func TestForeignChatIgnored(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(pressUpdate(1, 999, 999, "silence"))
	env.fake.push(pressUpdate(2, chatID, 999, "silence")) // right chat, wrong sender
	env.fake.push(`{"update_id":3,"message":{"chat":{"id":42},"from":{"id":42},"text":"hello"}}`)
	c := env.fake.waitCalls(1)
	if len(c) != 1 || c[0].Method != "sendMessage" || messageText(c[0]) != "✅ Alarme connectée." {
		t.Fatalf("calls = %v", c)
	}
	if env.sil.count() != 0 {
		t.Fatalf("Silence called %d times", env.sil.count())
	}
}

func TestFreshSetOffNeverRateLimited(t *testing.T) {
	env := startNotifier(t, nil)
	env.n.Alert(alarm.Alert{SensorID: 2})
	*env.now = fixedNow.Add(40 * time.Second)
	env.n.Alert(alarm.Alert{SensorID: 2}) // new set-off after a silence
	env.n.Alert(alarm.Alert{SensorID: 4}) // sentinel
	c := env.fake.waitMessage("Capteur 4")
	if n := countMessages(c, "Capteur 2"); n != 2 {
		t.Fatalf("sensor 2 messages = %d, want 2 (%v)", n, methods(c))
	}
	// The fresh alert still counts for the limit of the next detection.
	*env.now = fixedNow.Add(60 * time.Second)
	env.n.Alert(alarm.Alert{SensorID: 2, Again: true})
	env.n.Alert(alarm.Alert{SensorID: 5}) // sentinel
	c = env.fake.waitMessage("Capteur 5")
	if n := countMessages(c, "Capteur 2"); n != 2 {
		t.Fatalf("sensor 2 messages = %d, want 2 (%v)", n, methods(c))
	}
}

func TestStaleUpdatesDroppedAtStartup(t *testing.T) {
	env := startNotifierWith(t, nil, func(f *fakeTelegram) {
		f.push(pressUpdate(7, chatID, chatID, "silence"))
	})
	offs := env.fake.getOffsets()
	if offs[0] != -1 || offs[1] != 8 {
		t.Fatalf("offsets = %v, want [-1 8 ...]", offs)
	}
	c := env.fake.waitCalls(1)
	if c[0].Method != "answerCallbackQuery" || !strings.Contains(c[0].Body, "Appui ignoré") {
		t.Fatalf("call0 = %v", c[0])
	}
	if env.sil.count() != 0 {
		t.Fatalf("Silence called %d times", env.sil.count())
	}
}

func TestRetryGivesUp(t *testing.T) {
	oldGive := retryGiveUp
	retryGiveUp = 30 * time.Millisecond
	t.Cleanup(func() { retryGiveUp = oldGive })
	env := startNotifier(t, nil)
	env.fake.mu.Lock()
	env.fake.msgFail = 1 << 30
	env.fake.mu.Unlock()
	env.n.Alert(alarm.Alert{SensorID: 1})
	env.fake.waitMessage("Capteur 1")
	time.Sleep(150 * time.Millisecond) // well past giveUp
	env.fake.mu.Lock()
	env.fake.msgFail = 0
	env.fake.mu.Unlock()
	failed := countMessages(env.fake.snapshot(), "Capteur 1")
	env.n.Alert(alarm.Alert{SensorID: 2})
	c := env.fake.waitCalls(failed + 2) // + message and photo of sensor 2
	if n := countMessages(c, "Capteur 1"); n != failed {
		t.Fatalf("sensor 1 kept being retried: %d > %d", n, failed)
	}
	if countMessages(c, "Capteur 2") != 1 || countMethod(c, "sendPhoto") != 1 {
		t.Fatalf("calls = %v", methods(c))
	}
}

func TestTimeShownInParis(t *testing.T) {
	env := startNotifier(t, nil)
	*env.now = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) // CEST = UTC+2
	env.n.Alert(alarm.Alert{SensorID: 1})
	c := env.fake.waitMessage("Capteur 1")
	if want := "Capteur 1 — 14:00:00"; !strings.Contains(messageText(c[0]), want) {
		t.Fatalf("text = %q, want %q", messageText(c[0]), want)
	}
}

const chatB = 77

// callsTo returns the calls of the given method whose chat_id is chat.
func callsTo(c []fakeCall, method string, chat int64) []fakeCall {
	var out []fakeCall
	for _, x := range c {
		if x.Method != method {
			continue
		}
		if method == "sendPhoto" || method == "sendVideo" {
			if strings.Contains(x.Body, fmt.Sprintf("name=\"chat_id\"\r\n\r\n%d\r\n", chat)) {
				out = append(out, x)
			}
			continue
		}
		var b struct {
			ChatID int64 `json:"chat_id"`
		}
		if err := json.Unmarshal([]byte(x.Body), &b); err != nil {
			continue // not a JSON body: no chat_id to match
		}
		if b.ChatID == chat {
			out = append(out, x)
		}
	}
	return out
}

func TestAlertSentToEveryChat(t *testing.T) {
	var snaps atomic.Int32
	env := startNotifierChats(t, func(context.Context) ([]byte, error) {
		snaps.Add(1)
		return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
	}, nil, chatID, chatB)
	env.n.Alert(alarm.Alert{SensorID: 1})
	c := env.fake.waitCalls(4)
	for _, chat := range []int64{chatID, chatB} {
		if len(callsTo(c, "sendMessage", chat)) != 1 || len(callsTo(c, "sendPhoto", chat)) != 1 {
			t.Fatalf("chat %d calls = %v", chat, methods(c))
		}
		if !strings.Contains(callsTo(c, "sendMessage", chat)[0].Body, "silence") {
			t.Fatalf("chat %d: no silence button", chat)
		}
	}
	if n := snaps.Load(); n != 1 {
		t.Fatalf("Snapshot called %d times, want 1", n)
	}
}

func TestFailingChatDoesNotDelayOthers(t *testing.T) {
	env := startNotifierChats(t, nil, func(f *fakeTelegram) { f.failChat = chatID }, chatID, chatB)
	start := time.Now()
	env.n.Alert(alarm.Alert{SensorID: 1})
	deadline := time.Now().Add(time.Second)
	for len(callsTo(env.fake.snapshot(), "sendMessage", chatB)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("chat B not served in time: %v", methods(env.fake.snapshot()))
		}
		time.Sleep(time.Millisecond)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestSilenceByOneNotifiesAll(t *testing.T) {
	env := startNotifierChats(t, nil, nil, chatID, chatB)
	env.fake.push(fmt.Sprintf(`{"update_id":1,"callback_query":{"id":"cb1","from":{"id":%d,"first_name":"Julie"},"data":"silence","message":{"chat":{"id":%d}}}}`, chatB, chatB))
	c := env.fake.waitCalls(3)
	if c[0].Method != "answerCallbackQuery" || !strings.Contains(c[0].Body, "Sirène coupée") {
		t.Fatalf("call0 = %v", c[0])
	}
	const want = "🔕 Sirène coupée par Julie. L'alarme reste armée."
	for _, chat := range []int64{chatID, chatB} {
		m := callsTo(c, "sendMessage", chat)
		if len(m) != 1 || messageText(m[0]) != want {
			t.Fatalf("chat %d messages = %v", chat, m)
		}
	}
	if env.sil.count() != 1 {
		t.Fatalf("Silence calls = %d", env.sil.count())
	}
}

func TestSilenceWithoutNameNotifiesAll(t *testing.T) {
	env := startNotifierChats(t, nil, nil, chatID, chatB)
	env.fake.push(pressUpdate(1, chatID, chatID, "silence"))
	c := env.fake.waitCalls(3)
	for _, chat := range []int64{chatID, chatB} {
		m := callsTo(c, "sendMessage", chat)
		if len(m) != 1 || messageText(m[0]) != "🔕 Sirène coupée. L'alarme reste armée." {
			t.Fatalf("chat %d messages = %v", chat, m)
		}
	}
}

func TestNothingToStopOnlyToPresser(t *testing.T) {
	env := startNotifierChats(t, nil, nil, chatID, chatB)
	env.sil.result = false
	env.fake.push(pressUpdate(1, chatB, chatB, "silence"))
	env.fake.push(fmt.Sprintf(`{"update_id":2,"message":{"chat":{"id":%d},"from":{"id":%d},"text":"hello"}}`, chatID, chatID))
	// The text reply to A is a sentinel: it comes after the press handling.
	c := env.fake.waitMessage("Alarme connectée")
	m := callsTo(c, "sendMessage", chatB)
	if len(m) != 1 || messageText(m[0]) != "ℹ️ Rien à couper : l'alarme n'est pas déclenchée." {
		t.Fatalf("B messages = %v", m)
	}
	a := callsTo(c, "sendMessage", chatID)
	if len(a) != 1 || messageText(a[0]) != "✅ Alarme connectée." {
		t.Fatalf("A messages = %v", a)
	}
}

func TestGroupMessageFromListedAccountIgnored(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(pressUpdate(1, -100, chatID, "silence")) // listed account, from a group
	env.fake.push(`{"update_id":2,"message":{"chat":{"id":-100},"from":{"id":42},"text":"hi"}}`)
	env.fake.push(`{"update_id":3,"message":{"chat":{"id":42},"from":{"id":42},"text":"hello"}}`)
	c := env.fake.waitCalls(1)
	if len(c) != 1 || messageText(c[0]) != "✅ Alarme connectée." || env.sil.count() != 0 {
		t.Fatalf("calls = %v silences=%d", c, env.sil.count())
	}
}

func TestStaleSilenceFromSecondAccountAnsweredToPresserOnly(t *testing.T) {
	env := startNotifierChats(t, nil, func(f *fakeTelegram) {
		f.push(pressUpdate(7, chatB, chatB, "silence"))
	}, chatID, chatB)
	c := env.fake.waitCalls(1)
	if c[0].Method != "answerCallbackQuery" || !strings.Contains(c[0].Body, "Appui ignoré") || env.sil.count() != 0 {
		t.Fatalf("calls = %v", c)
	}
}

// --- arming and status ---

func pressFrom(id int64, chat int64, name, data string) string {
	return fmt.Sprintf(`{"update_id":%d,"callback_query":{"id":"cb%d","from":{"id":%d,"first_name":%q},"data":%q,"message":{"chat":{"id":%d}}}}`, id, id, chat, name, data, chat)
}

func textFrom(id int64, chat int64, name, text string) string {
	return fmt.Sprintf(`{"update_id":%d,"message":{"chat":{"id":%d},"from":{"id":%d,"first_name":%q},"text":%q}}`, id, chat, chat, name, text)
}

type replyMarkup struct {
	Keyboard []any `json:"keyboard"`
	Inline   [][]struct {
		Text string `json:"text"`
		Data string `json:"callback_data"`
	} `json:"inline_keyboard"`
}

func markupOf(c fakeCall) replyMarkup {
	var b struct {
		ReplyMarkup replyMarkup `json:"reply_markup"`
	}
	if err := json.Unmarshal([]byte(c.Body), &b); err != nil {
		return replyMarkup{} // the caller's assertions then fail
	}
	return b.ReplyMarkup
}

func TestUnknownTextRepliesWithKeyboard(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(textFrom(1, chatID, "Guillaume", "/start"))
	c := env.fake.waitCalls(1)
	if messageText(c[0]) != "✅ Alarme connectée." {
		t.Fatalf("text = %q", messageText(c[0]))
	}
	kb := markupOf(c[0]).Keyboard
	if len(kb) != 3 || len(kb[0].([]any)) != 2 || len(kb[1].([]any)) != 2 || len(kb[2].([]any)) != 1 {
		t.Fatalf("keyboard = %v", kb)
	}
	key := func(r, i int) any { return kb[r].([]any)[i].(map[string]any)["text"] }
	if key(0, 0) != "🔒 Armer" || key(0, 1) != "🌙 Armer (nuit)" || key(1, 0) != "📷 Photo" || key(1, 1) != "🎥 Vidéo" || key(2, 0) != "ℹ️ État" {
		t.Fatalf("keyboard = %v", kb)
	}
	if !strings.Contains(c[0].Body, `"is_persistent":true`) {
		t.Fatalf("body = %s", c[0].Body)
	}
}

func TestArmAllClosed(t *testing.T) {
	env := startNotifierChats(t, nil, nil, chatID, chatB)
	env.fake.push(textFrom(1, chatB, "Julie", "🔒 Armer"))
	c := env.fake.waitCalls(2)
	for _, chat := range []int64{chatID, chatB} {
		m := callsTo(c, "sendMessage", chat)
		if len(m) != 1 || messageText(m[0]) != "🔒 Alarme armée (total) par Julie." {
			t.Fatalf("chat %d messages = %v", chat, m)
		}
	}
	if got := env.sil.arms(); len(got) != 1 || got[0] != [2]bool{false, false} {
		t.Fatalf("arms = %v", got)
	}
}

func TestArmNightCommand(t *testing.T) {
	env := startNotifier(t, nil)
	env.sil.armResult = alarm.ArmSwitched
	env.fake.push(textFrom(1, chatID, "", "/Nuit@MonBot"))
	c := env.fake.waitCalls(1)
	if messageText(c[0]) != "🌙 Alarme armée (nuit)." {
		t.Fatalf("text = %q", messageText(c[0]))
	}
	if got := env.sil.arms(); len(got) != 1 || got[0] != [2]bool{true, false} {
		t.Fatalf("arms = %v", got)
	}
}

func openSensors(ids ...int) func(bool) []int {
	return func(bool) []int { return ids }
}

func TestArmWithOpenAsksFirst(t *testing.T) {
	env := startNotifierChats(t, nil, nil, chatID, chatB)
	env.sil.inAlarm = openSensors(22)
	env.fake.push(textFrom(1, chatB, "Julie", "/armer"))
	c := env.fake.waitCalls(1)
	want := "⚠️ Capteur 22 : en alerte (ouvert ou mouvement). Armer quand même ? Ces capteurs seront ignorés jusqu'à leur retour au repos."
	if messageText(c[0]) != want {
		t.Fatalf("text = %q", messageText(c[0]))
	}
	in := markupOf(c[0]).Inline
	if len(in) != 1 || len(in[0]) != 2 || in[0][0].Text != "Armer quand même" || in[0][0].Data != "arm_force:22" || in[0][1].Text != "Annuler" || in[0][1].Data != "arm_cancel" {
		t.Fatalf("buttons = %+v", in)
	}
	if len(env.sil.arms()) != 0 {
		t.Fatalf("armed before confirmation: %v", env.sil.arms())
	}
	env.fake.push(pressFrom(2, chatB, "Julie", "arm_force:22"))
	c = env.fake.waitCalls(4) // warning, answer, 2 broadcasts
	if got := env.sil.arms(); len(got) != 1 || got[0] != [2]bool{false, true} {
		t.Fatalf("arms = %v", got)
	}
	if n := countMessages(c, "🔒 Alarme armée (total) par Julie."); n != 2 {
		t.Fatalf("broadcasts = %d (%v)", n, methods(c))
	}
	if len(callsTo(c, "sendMessage", chatID)) != 1 {
		t.Fatalf("chat A got the warning: %v", c)
	}
}

func TestArmForceRechecks(t *testing.T) {
	env := startNotifier(t, nil)
	env.sil.inAlarm = openSensors(22)
	env.fake.push(textFrom(1, chatID, "G", "🌙 Armer (nuit)"))
	c := env.fake.waitCalls(1)
	if markupOf(c[0]).Inline[0][0].Data != "arm_night_force:22" {
		t.Fatalf("data = %v", markupOf(c[0]).Inline)
	}
	env.sil.mu.Lock()
	env.sil.inAlarm = openSensors(22, 28)
	env.sil.mu.Unlock()
	env.fake.push(pressFrom(2, chatID, "G", "arm_night_force:22"))
	c = env.fake.waitCalls(3) // warning, answer, new warning
	last := c[len(c)-1]
	if last.Method != "sendMessage" || !strings.Contains(messageText(last), "Capteur 22, Capteur 28 : en alerte") {
		t.Fatalf("last = %v", last)
	}
	if d := markupOf(last).Inline[0][0].Data; d != "arm_night_force:22,28" {
		t.Fatalf("data = %q", d)
	}
	if len(env.sil.arms()) != 0 {
		t.Fatalf("armed: %v", env.sil.arms())
	}
}

func TestArmWarningTruncatesLongIDList(t *testing.T) {
	env := startNotifier(t, nil)
	var ids []int
	for i := 100; i < 140; i++ {
		ids = append(ids, i)
	}
	env.sil.inAlarm = openSensors(ids...)
	env.fake.push(textFrom(1, chatID, "G", "/armer"))
	c := env.fake.waitCalls(1)
	d := markupOf(c[0]).Inline[0][0].Data
	if len(d) > 64 || !strings.HasPrefix(d, "arm_force:100,101") {
		t.Fatalf("data = %q (%d bytes)", d, len(d))
	}
	// Pressing it: ids beyond the data count as new, so no arming.
	env.fake.push(pressFrom(2, chatID, "G", d))
	env.fake.waitCalls(3)
	if len(env.sil.arms()) != 0 {
		t.Fatalf("armed: %v", env.sil.arms())
	}
}

func TestArmForceMalformed(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(pressFrom(1, chatID, "G", "arm_force:"))
	env.fake.push(pressFrom(2, chatID, "G", "arm_force:2x"))
	env.fake.push(pressFrom(3, chatID, "G", "arm_night_force:1,,2"))
	c := env.fake.waitCalls(3)
	for _, x := range c {
		if x.Method != "answerCallbackQuery" || strings.Contains(x.Body, `"text"`) {
			t.Fatalf("call = %v", x)
		}
	}
	if len(env.sil.arms()) != 0 {
		t.Fatalf("armed: %v", env.sil.arms())
	}
}

func TestArmCancel(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(pressFrom(1, chatID, "G", "arm_cancel"))
	c := env.fake.waitCalls(2)
	if c[0].Method != "answerCallbackQuery" || !strings.Contains(c[0].Body, "Annulé") {
		t.Fatalf("call0 = %v", c[0])
	}
	if messageText(c[1]) != "Armement annulé." {
		t.Fatalf("call1 = %v", c[1])
	}
	if len(env.sil.arms()) != 0 {
		t.Fatalf("armed: %v", env.sil.arms())
	}
}

func TestArmAlreadyAndBusy(t *testing.T) {
	env := startNotifier(t, nil)
	env.sil.armResult = alarm.ArmAlready
	env.fake.push(textFrom(1, chatID, "G", "/nuit"))
	c := env.fake.waitCalls(1)
	if messageText(c[0]) != "ℹ️ L'alarme est déjà armée (nuit)." {
		t.Fatalf("text = %q", messageText(c[0]))
	}
	env.sil.mu.Lock()
	env.sil.armResult = alarm.ArmBusy
	env.sil.snap = alarm.Snapshot{State: alarm.StatePending}
	env.sil.mu.Unlock()
	env.fake.push(textFrom(2, chatID, "G", "/armer"))
	c = env.fake.waitCalls(2)
	if messageText(c[1]) != "⚠️ Impossible d'armer maintenant : l'alarme est en délai d'entrée." {
		t.Fatalf("text = %q", messageText(c[1]))
	}
}

func TestStatus(t *testing.T) {
	env := startNotifier(t, nil)
	env.sil.snap = alarm.Snapshot{State: alarm.StateArmedAway, ArmedAt: time.Date(2026, 10, 18, 12, 5, 0, 0, time.UTC).Unix()}
	env.sil.inAlarm = openSensors(22)
	env.fake.push(textFrom(1, chatID, "G", "ℹ️ État"))
	c := env.fake.waitCalls(1)
	if want := "🔒 Armée (total) depuis 14:05.\nEn alerte : Capteur 22."; messageText(c[0]) != want {
		t.Fatalf("text = %q", messageText(c[0]))
	}
	if len(markupOf(c[0]).Keyboard) != 3 {
		t.Fatalf("no keyboard: %s", c[0].Body)
	}
	env.sil.mu.Lock()
	env.sil.snap = alarm.Snapshot{State: alarm.StateDisarmed}
	env.sil.inAlarm = nil
	env.sil.mu.Unlock()
	env.fake.push(textFrom(2, chatID, "G", "/etat"))
	c = env.fake.waitCalls(2)
	if want := "ℹ️ Désarmée.\nTout est fermé."; messageText(c[1]) != want {
		t.Fatalf("text = %q", messageText(c[1]))
	}
}

func TestDisarmCommandRefused(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(textFrom(1, chatID, "G", "/desarmer"))
	c := env.fake.waitCalls(1)
	if messageText(c[0]) != "🔐 Le désarmement se fait uniquement au clavier." {
		t.Fatalf("text = %q", messageText(c[0]))
	}
	if len(env.sil.arms()) != 0 || env.sil.count() != 0 {
		t.Fatal("engine mutated")
	}
}

func TestForeignChatCannotArm(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(textFrom(1, 999, "X", "🔒 Armer"))
	env.fake.push(pressFrom(2, 999, "X", "arm_force:22"))
	env.fake.push(textFrom(3, chatID, "G", "hello")) // sentinel
	env.fake.waitCalls(1)
	if len(env.sil.arms()) != 0 {
		t.Fatalf("armed: %v", env.sil.arms())
	}
}

func TestStaleArmForceNotActedOn(t *testing.T) {
	env := startNotifierWith(t, nil, func(f *fakeTelegram) {
		f.push(pressFrom(7, chatID, "G", "arm_force:22"))
	})
	c := env.fake.waitCalls(1)
	if c[0].Method != "answerCallbackQuery" || !strings.Contains(c[0].Body, "Appui ignoré") {
		t.Fatalf("call0 = %v", c[0])
	}
	if len(env.sil.arms()) != 0 {
		t.Fatalf("armed: %v", env.sil.arms())
	}
}

// --- photo, video and alert clips ---

// fakeClip records a clip once its gate is closed, or fails with ctx.
type fakeClip struct {
	gate  chan struct{}
	err   atomic.Pointer[error] // returned once the gate opens
	calls atomic.Int32
}

func (f *fakeClip) fail(err error) { f.err.Store(&err) }

func newFakeClip() *fakeClip { return &fakeClip{gate: make(chan struct{})} }

func (f *fakeClip) record(ctx context.Context) (clip.Clip, error) {
	n := f.calls.Add(1)
	select {
	case <-f.gate:
	case <-ctx.Done():
		return clip.Clip{}, ctx.Err()
	}
	if err := f.err.Load(); err != nil {
		return clip.Clip{}, *err
	}
	return clip.Clip{MP4: []byte(fmt.Sprintf("mp4-%d", n)), Width: 1280, Height: 720, Duration: 15 * time.Second}, nil
}

func startWithClip(t *testing.T, fc *fakeClip, chats ...int64) *notifierEnv {
	t.Helper()
	return startNotifierOpts(t, nil, nil, func(o *Options) { o.Clip = fc.record }, chats...)
}

// formField returns a field of a multipart body ("" if absent).
func formField(body, name string) string {
	_, after, ok := strings.Cut(body, `name="`+name+`"`)
	if !ok {
		return ""
	}
	_, after, _ = strings.Cut(after, "\r\n\r\n")
	v, _, _ := strings.Cut(after, "\r\n")
	return v
}

// waitMethod waits until n calls of method were recorded.
func (f *fakeTelegram) waitMethod(method string, n int) []fakeCall {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := f.snapshot()
		if countMethod(c, method) >= n {
			return c
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("timeout waiting for %d %s, got %v", n, method, methods(c))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestAlertTextNotDelayedByClip(t *testing.T) {
	fc := newFakeClip()
	env := startWithClip(t, fc, chatID)
	env.n.Alert(alarm.Alert{SensorID: 1})
	c := env.fake.waitCalls(2) // the clip is still blocked
	if got := methods(c); got[0] != "sendMessage" || got[1] != "sendPhoto" {
		t.Fatalf("calls = %v", got)
	}
	if fc.calls.Load() != 1 {
		t.Fatalf("Clip calls = %d", fc.calls.Load())
	}
	close(fc.gate)
	c = env.fake.waitMethod("sendVideo", 1)
	v := callsTo(c, "sendVideo", chatID)
	if len(v) != 1 || formField(v[0].Body, "caption") != "🎥 Alerte — 14:03:09" || formField(v[0].Body, "video") != "mp4-1" {
		t.Fatalf("videos = %v", v)
	}
	if got := methods(c); got[len(got)-1] != "sendVideo" || len(got) != 3 {
		t.Fatalf("calls = %v", got)
	}
}

func TestNextAlertNotDelayedByClip(t *testing.T) {
	fc := newFakeClip()
	env := startWithClip(t, fc, chatID)
	env.n.Alert(alarm.Alert{SensorID: 1})
	env.n.Alert(alarm.Alert{SensorID: 2})
	c := env.fake.waitMessage("Capteur 2") // never released
	if countMessages(c, "Capteur 1") != 1 || countMethod(c, "sendVideo") != 0 {
		t.Fatalf("calls = %v", methods(c))
	}
}

func TestOneRecordingSharedAcrossAlertsAndChats(t *testing.T) {
	fc := newFakeClip()
	var logs syncBuffer
	env := startNotifierOpts(t, nil, nil, func(o *Options) {
		o.Clip = fc.record
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	}, chatID, chatB)
	env.n.Alert(alarm.Alert{SensorID: 1})
	env.n.Alert(alarm.Alert{SensorID: 2})
	env.n.Alert(alarm.Alert{SensorID: 3}) // sentinel: alert 2 fully handled first
	// Every alert handled by both senders while the recording is held.
	env.fake.waitMethod("sendPhoto", 6)
	// A duplicate would have been caught by the in-flight cap instead.
	if strings.Contains(logs.String(), "alert clip skipped") {
		t.Fatalf("shared clip not deduplicated: %s", logs.String())
	}
	close(fc.gate)
	c := env.fake.waitMethod("sendVideo", 2)
	if n := fc.calls.Load(); n != 1 {
		t.Fatalf("Clip calls = %d, want 1", n)
	}
	for _, chat := range []int64{chatID, chatB} {
		v := callsTo(c, "sendVideo", chat)
		if len(v) != 1 || formField(v[0].Body, "video") != "mp4-1" {
			t.Fatalf("chat %d videos = %v", chat, v)
		}
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestClipFailureDoesNotBreakAlert(t *testing.T) {
	fc := newFakeClip()
	fc.fail(errors.New("no IDR"))
	close(fc.gate)
	var logs syncBuffer
	env := startNotifierOpts(t, nil, nil, func(o *Options) {
		o.Clip = fc.record
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	}, chatID)
	env.n.Alert(alarm.Alert{SensorID: 1})
	env.fake.waitMethod("sendPhoto", 1)
	env.n.Alert(alarm.Alert{SensorID: 2}) // sentinel
	c := env.fake.waitMethod("sendPhoto", 2)
	if countMessages(c, "Capteur 1") != 1 || countMessages(c, "Capteur 2") != 1 || countMethod(c, "sendVideo") != 0 {
		t.Fatalf("calls = %v", methods(c))
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "clip") {
		if time.Now().After(deadline) {
			t.Fatalf("no warning: %s", logs.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestPhotoButton(t *testing.T) {
	var fail atomic.Bool
	env := startNotifierChats(t, func(context.Context) ([]byte, error) {
		if fail.Load() {
			return nil, errors.New("no camera")
		}
		return []byte{0xff, 0xd8, 0xff, 0xd9}, nil
	}, nil, chatID, chatB)
	env.fake.push(textFrom(1, chatB, "Julie", "📷 Photo"))
	c := env.fake.waitCalls(1)
	p := callsTo(c, "sendPhoto", chatB)
	if len(c) != 1 || len(p) != 1 || formField(p[0].Body, "caption") != "📷 14:03:09" {
		t.Fatalf("calls = %v", c)
	}
	fail.Store(true)
	env.fake.push(textFrom(2, chatB, "Julie", "/photo"))
	c = env.fake.waitCalls(2)
	if c[1].Method != "sendMessage" || messageText(c[1]) != "⚠️ Photo impossible pour le moment." || len(callsTo(c, "sendMessage", chatB)) != 1 {
		t.Fatalf("calls = %v", c)
	}
}

func TestVideoButton(t *testing.T) {
	fc := newFakeClip()
	close(fc.gate)
	env := startWithClip(t, fc, chatID, chatB)
	env.fake.push(textFrom(1, chatB, "Julie", "🎥 Vidéo"))
	c := env.fake.waitCalls(2)
	if c[0].Method != "sendMessage" || messageText(c[0]) != "🎥 Enregistrement de 15 s…" || len(callsTo(c, "sendMessage", chatB)) != 1 {
		t.Fatalf("call0 = %v", c[0])
	}
	v := callsTo(c, "sendVideo", chatB)
	if len(c) != 2 || len(v) != 1 || formField(v[0].Body, "caption") != "🎥 14:03:09" || formField(v[0].Body, "video") != "mp4-1" {
		t.Fatalf("calls = %v", c)
	}
	fc.fail(errors.New("no IDR")) // read by the next recording only
	env.fake.push(textFrom(2, chatB, "Julie", "/VIDEO"))
	c = env.fake.waitCalls(4)
	if messageText(c[2]) != "🎥 Enregistrement de 15 s…" || messageText(c[3]) != "⚠️ Vidéo impossible pour le moment." || len(callsTo(c, "sendMessage", chatID)) != 0 {
		t.Fatalf("calls = %v", c)
	}
}

func TestVideoWithoutClip(t *testing.T) {
	env := startNotifier(t, nil)
	env.fake.push(textFrom(1, chatID, "G", "/video"))
	c := env.fake.waitCalls(1)
	if messageText(c[0]) != "⚠️ Vidéo impossible pour le moment." {
		t.Fatalf("calls = %v", c)
	}
}

func TestVideoJoinsAlertRecording(t *testing.T) {
	fc := newFakeClip()
	env := startWithClip(t, fc, chatID)
	env.n.Alert(alarm.Alert{SensorID: 1})
	env.fake.waitMethod("sendPhoto", 1)
	env.fake.push(textFrom(1, chatID, "G", "🎥 Vidéo"))
	env.fake.waitMessage("Enregistrement de 15 s")
	close(fc.gate)
	c := env.fake.waitMethod("sendVideo", 2)
	if n := fc.calls.Load(); n != 1 {
		t.Fatalf("Clip calls = %d, want 1", n)
	}
	captions := map[string]bool{}
	for _, v := range callsTo(c, "sendVideo", chatID) {
		if formField(v.Body, "video") != "mp4-1" {
			t.Fatalf("video = %q", formField(v.Body, "video"))
		}
		captions[formField(v.Body, "caption")] = true
	}
	if !captions["🎥 Alerte — 14:03:09"] || !captions["🎥 14:03:09"] {
		t.Fatalf("captions = %v", captions)
	}
}

func TestPollNotBlockedByVideo(t *testing.T) {
	fc := newFakeClip()
	env := startWithClip(t, fc, chatID)
	env.fake.push(textFrom(1, chatID, "G", "🎥 Vidéo"))
	env.fake.waitMessage("Enregistrement de 15 s")
	env.fake.push(textFrom(2, chatID, "G", "ℹ️ État"))
	c := env.fake.waitMessage("Tout est fermé")
	if countMethod(c, "sendVideo") != 0 {
		t.Fatalf("calls = %v", methods(c))
	}
}

func TestRunReturnsWithPendingClip(t *testing.T) {
	fc := newFakeClip()
	env := startWithClip(t, fc, chatID)
	env.n.Alert(alarm.Alert{SensorID: 1})
	env.fake.push(textFrom(1, chatID, "G", "🎥 Vidéo"))
	env.fake.waitMethod("sendPhoto", 1)
	env.fake.waitMessage("Enregistrement de 15 s")
	env.cancel()
	select {
	case <-env.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2 s with a pending clip")
	}
}

func TestAlertClipUploadsCapped(t *testing.T) {
	fc := newFakeClip()
	close(fc.gate) // every recording finishes at once
	var logs syncBuffer
	env := startNotifierOpts(t, nil, func(f *fakeTelegram) { f.hangVid = make(chan struct{}) }, func(o *Options) {
		o.Clip = fc.record
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	}, chatID)
	env.n.Alert(alarm.Alert{SensorID: 1})
	env.fake.waitMethod("sendVideo", 1) // first clip upload, stuck
	// Recording 1 is over, so alert 2 gets a new recording, whose clip must
	// be skipped while the first upload is in flight.
	env.n.Alert(alarm.Alert{SensorID: 2})
	env.n.Alert(alarm.Alert{SensorID: 3}) // sentinel: alert 2 fully handled first
	c := env.fake.waitMessage("Capteur 3")
	if countMessages(c, "Capteur 1") != 1 || countMessages(c, "Capteur 2") != 1 {
		t.Fatalf("calls = %v", methods(c))
	}
	if n := fc.calls.Load(); n < 2 {
		t.Fatalf("Clip calls = %d, want a second recording", n)
	}
	if !strings.Contains(logs.String(), "alert clip skipped, previous upload in flight") {
		t.Fatalf("no skip logged: %s", logs.String())
	}
	if n := countMethod(env.fake.snapshot(), "sendVideo"); n != 1 {
		t.Fatalf("sendVideo calls = %d, want 1", n)
	}
}
