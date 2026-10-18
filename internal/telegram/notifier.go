package telegram

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // Europe/Paris on a camera without zoneinfo

	"github.com/caligone/openqiara/internal/alarm"
	"github.com/caligone/openqiara/internal/clip"
)

// Package-level knobs, variables so tests can shorten them.
var (
	retryInitial    = 5 * time.Second  // first backoff when the alert text fails
	retryMax        = 60 * time.Second // backoff cap
	retryGiveUp     = 12 * time.Hour   // stop retrying one alert text after this
	pollTimeout     = 50 * time.Second // getUpdates long-poll timeout
	pollPause       = 5 * time.Second  // first pause after a getUpdates error
	pollPauseMax    = 5 * time.Minute  // cap of the getUpdates error backoff
	snapshotTimeout = 15 * time.Second // snapshot + photo upload budget
)

const (
	queueSize     = 16
	rateInterval  = time.Minute
	silenceButton = "silence"

	armForce      = "arm_force:"
	armNightForce = "arm_night_force:"
	armCancel     = "arm_cancel"
	maxCallback   = 64 // Telegram's callback_data limit, in bytes

	btnArm    = "🔒 Armer"
	btnNight  = "🌙 Armer (nuit)"
	btnPhoto  = "📷 Photo"
	btnVideo  = "🎥 Vidéo"
	btnStatus = "ℹ️ État"

	msgPhotoFailed = "⚠️ Photo impossible pour le moment."
	msgVideoFailed = "⚠️ Vidéo impossible pour le moment."
)

var mainKeyboard = Keyboard{{btnArm, btnNight}, {btnPhoto, btnVideo}, {btnStatus}}

// alertZone is the zone of the times shown in messages: Go ignores the
// camera's uClibc TZ, so time.Local may be UTC.
var alertZone = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Paris"); err == nil {
		return loc
	}
	return time.Local
}()

// retryLogEvery throttles the "alert text failed" warning to one attempt in N.
const retryLogEvery = 10

// Alarm is what the notifier needs from the engine; *alarm.Engine implements
// it. There is deliberately no way to disarm.
type Alarm interface {
	Silence() bool
	ArmRemote(night, ignoreOpen bool) alarm.ArmResult
	InAlarm(night bool) []int
	Snapshot() alarm.Snapshot
}

// Options configures a Notifier.
type Options struct {
	ChatIDs  []int64 // authorized private chats, each gets its own queue and sender
	Alarm    Alarm
	Snapshot func(ctx context.Context) ([]byte, error)
	Clip     func(ctx context.Context) (clip.Clip, error) // records the next 15 s; nil: no clips
	Name     func(sensorID int) string                    // readable sensor name
	Now      func() time.Time                             // nil: time.Now
	Logger   *slog.Logger                                 // nil: slog.Default()
}

// photoOnce takes the snapshot once per alert, whichever chat's sender
// needs it first, and shares it between the chats.
type photoOnce struct {
	once sync.Once
	jpeg []byte
	err  error
}

func (p *photoOnce) get(ctx context.Context, snap func(context.Context) ([]byte, error)) ([]byte, error) {
	p.once.Do(func() {
		pctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		defer cancel()
		p.jpeg, p.err = snap(pctx)
	})
	return p.jpeg, p.err
}

// recording is a shared future: one Clip call whose result every alert and
// video request that joined it waits for.
type recording struct {
	id   uint64        // distinct per recording, so senders can skip duplicates
	done chan struct{} // closed once clip and err are set
	clip clip.Clip
	err  error
}

type queued struct {
	alert alarm.Alert
	at    time.Time
	photo *photoOnce
}

// Notifier sends alarm alerts to Telegram and handles the "silence" button.
type Notifier struct {
	bot    *Bot
	o      Options
	log    *slog.Logger
	chats  []int64
	queues map[int64]chan queued

	mu   sync.Mutex
	last map[int]time.Time // last accepted alert per sensor

	wg sync.WaitGroup // every goroutine of Run

	recMu  sync.Mutex
	rec    *recording // in progress, nil if none
	recSeq uint64
}

// NewNotifier builds a Notifier. Call Run to start it.
func NewNotifier(bot *Bot, o Options) *Notifier {
	if o.Now == nil {
		o.Now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	n := &Notifier{bot: bot, o: o, log: log, queues: map[int64]chan queued{}, last: map[int]time.Time{}}
	for _, id := range o.ChatIDs {
		if _, dup := n.queues[id]; !dup {
			n.queues[id] = make(chan queued, queueSize)
			n.chats = append(n.chats, id)
		}
	}
	return n
}

func (n *Notifier) authorized(id int64) bool {
	_, ok := n.queues[id]
	return ok
}

// Alert queues an alert for every chat. It never blocks and never calls back
// into the engine, so it is safe as the engine's alert hook (called under its
// lock).
func (n *Notifier) Alert(a alarm.Alert) {
	at := n.o.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	// Only detections while already triggered are rate-limited: a fresh
	// set-off (including after a silence) is always sent.
	if prev, ok := n.last[a.SensorID]; a.Again && ok && at.Sub(prev) < rateInterval {
		n.log.Info("telegram: alert rate-limited", "sensor", a.SensorID)
		return
	}
	q := queued{alert: a, at: at, photo: &photoOnce{}}
	for _, id := range n.chats {
		select {
		case n.queues[id] <- q:
			n.last[a.SensorID] = at
		default:
			n.log.Warn("telegram: alert queue full, alert dropped", "sensor", a.SensorID, "chat", id)
		}
	}
}

// Run sends queued alerts and long-polls for button presses until ctx ends.
// It returns once all its goroutines (loops, recordings, uploads) have
// stopped. Call it once.
func (n *Notifier) Run(ctx context.Context) {
	for _, id := range n.chats {
		n.spawn(func() { n.sendLoop(ctx, id) })
	}
	n.pollLoop(ctx)
	n.wg.Wait()
}

// spawn runs f in a goroutine that Run waits for. It is only called from
// Run's goroutines while they run, so the WaitGroup is never zero then.
func (n *Notifier) spawn(f func()) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		f()
	}()
}

// record joins the recording in progress, or starts one. It never blocks:
// the recording runs in its own goroutine. There is never more than one
// Clip call at a time, and the finished recording is forgotten so its bytes
// are freed once its uploads are done.
func (n *Notifier) record(ctx context.Context) *recording {
	n.recMu.Lock()
	defer n.recMu.Unlock()
	if n.rec != nil {
		return n.rec
	}
	n.recSeq++
	r := &recording{id: n.recSeq, done: make(chan struct{})}
	n.rec = r
	n.spawn(func() {
		r.clip, r.err = n.o.Clip(ctx)
		if r.err != nil && ctx.Err() == nil {
			n.log.Warn("telegram: clip recording failed", "err", r.err)
		}
		n.recMu.Lock()
		n.rec = nil
		n.recMu.Unlock()
		close(r.done)
	})
	return r
}

// wait returns the finished clip, or false if the recording failed or ctx
// ended first.
func (r *recording) wait(ctx context.Context) (clip.Clip, bool) {
	select {
	case <-ctx.Done():
		return clip.Clip{}, false
	case <-r.done:
		return r.clip, r.err == nil
	}
}

func (n *Notifier) sendLoop(ctx context.Context, chat int64) {
	queue := n.queues[chat]
	cs := &clipState{}
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-queue:
			n.send(ctx, chat, q, cs)
		}
	}
}

// clipState is a sender's alert-clip bookkeeping.
type clipState struct {
	last     uint64      // id of the last recording sent as an alert clip
	inFlight atomic.Bool // an alert clip of this chat is waited for or uploading
}

func (n *Notifier) text(q queued) string {
	head := "🚨 Alarme déclenchée"
	if q.alert.Again {
		head = "🚨 Nouvelle détection"
	}
	who := "la sirène"
	if q.alert.SensorID != 0 {
		who = n.o.Name(q.alert.SensorID)
	}
	return head + " — " + who + " — " + q.at.In(alertZone).Format("15:04:05")
}

// sleep waits d or until ctx ends; it reports whether the full wait elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// send delivers one alert to chat: text with the silence button, then the
// photo, then (in the background) the clip. The clip never delays the text
// nor the next alert. A recording shared by several alerts is sent only
// once, and a chat has at most one alert clip in flight: a newer one is
// skipped, so slow uploads cannot pile up clips in memory.
func (n *Notifier) send(ctx context.Context, chat int64, q queued, cs *clipState) {
	var rec *recording
	if n.o.Clip != nil {
		rec = n.record(ctx) // starts filming now, does not wait
	}
	text := n.text(q)
	if !n.sendText(ctx, chat, q, text) {
		return
	}
	n.sendAlertPhoto(ctx, chat, q, text)
	if rec == nil || rec.id == cs.last {
		return
	}
	if !cs.inFlight.CompareAndSwap(false, true) {
		n.log.Info("telegram: alert clip skipped, previous upload in flight", "chat", chat)
		return
	}
	cs.last = rec.id
	caption := "🎥 Alerte — " + q.at.In(alertZone).Format("15:04:05")
	n.spawn(func() {
		defer cs.inFlight.Store(false)
		c, ok := rec.wait(ctx) // a failed recording was logged once already
		if !ok {
			return
		}
		if err := n.bot.SendVideo(ctx, chat, c.MP4, caption, c.Width, c.Height, c.Duration); err != nil && ctx.Err() == nil {
			n.log.Warn("telegram: alert clip not sent", "sensor", q.alert.SensorID, "err", err)
		}
	})
}

// sendText sends the alert text, retrying with backoff; it reports whether
// it was delivered.
func (n *Notifier) sendText(ctx context.Context, chat int64, q queued, text string) bool {
	btn := &Button{Text: "🔕 Couper la sirène", Data: silenceButton}
	backoff := retryInitial
	start := time.Now()
	failures := 0
	for {
		err := n.bot.SendMessage(ctx, chat, text, btn)
		if err == nil {
			if failures > 0 {
				n.log.Info("telegram: alert text sent after retries", "sensor", q.alert.SensorID, "failures", failures)
			}
			return true
		}
		failures++
		if ctx.Err() != nil {
			return false
		}
		if time.Since(start)+backoff > retryGiveUp {
			n.log.Error("telegram: giving up on alert text", "sensor", q.alert.SensorID, "err", err)
			return false
		}
		if failures%retryLogEvery == 1 {
			n.log.Warn("telegram: alert text failed, retrying", "sensor", q.alert.SensorID, "err", err, "attempt", failures, "in", backoff)
		}
		if !sleep(ctx, backoff) {
			return false
		}
		if backoff *= 2; backoff > retryMax {
			backoff = retryMax
		}
	}
}

func (n *Notifier) sendAlertPhoto(ctx context.Context, chat int64, q queued, text string) {
	if n.o.Snapshot == nil {
		return
	}
	jpeg, err := q.photo.get(ctx, n.o.Snapshot)
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		defer cancel()
		err = n.bot.SendPhoto(pctx, chat, jpeg, text)
	}
	if err != nil && ctx.Err() == nil {
		n.log.Warn("telegram: alert photo not sent", "sensor", q.alert.SensorID, "err", err)
	}
}

// drain skips the updates Telegram kept while the daemon was down, so a stale
// "silence" press cannot cut a newer alarm. It returns the offset to poll from.
func (n *Notifier) drain(ctx context.Context) (offset int64, ok bool) {
	for {
		ups, err := n.bot.GetUpdates(ctx, -1, 0) // -1: only the last pending update
		if err == nil {
			for _, u := range ups {
				if u.ID >= offset {
					offset = u.ID + 1
				}
				if u.Callback != "" && (u.Data == silenceButton || strings.HasPrefix(u.Data, "arm_")) && n.authorized(u.ChatID) && u.FromID == u.ChatID {
					if err := n.bot.AnswerCallback(ctx, u.Callback, "Appui ignoré (redémarrage)"); err != nil && ctx.Err() == nil {
						n.log.Warn("telegram: answerCallback failed", "err", err)
					}
				}
			}
			return offset, true
		}
		if ctx.Err() != nil {
			return 0, false
		}
		n.log.Warn("telegram: dropping pending updates failed", "err", err)
		if !sleep(ctx, pollPause) {
			return 0, false
		}
	}
}

func (n *Notifier) pollLoop(ctx context.Context) {
	offset, ok := n.drain(ctx)
	if !ok {
		return
	}
	var (
		pause   = pollPause
		failing bool
		lastLog time.Time
	)
	for ctx.Err() == nil {
		ups, err := n.bot.GetUpdates(ctx, offset, pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !failing || time.Since(lastLog) >= pollPauseMax {
				n.log.Warn("telegram: getUpdates failed", "err", err, "retry_in", pause)
				lastLog = time.Now()
			}
			failing = true
			sleep(ctx, pause)
			if pause *= 2; pause > pollPauseMax {
				pause = pollPauseMax
			}
			continue
		}
		if failing {
			n.log.Info("telegram: getUpdates recovered")
			failing, pause = false, pollPause
		}
		for _, u := range ups {
			if u.ID >= offset {
				offset = u.ID + 1
			}
			n.handle(ctx, u)
		}
	}
}

func (n *Notifier) handle(ctx context.Context, u Update) {
	if u.ChatID == 0 && u.FromID == 0 {
		return // update kind we do not use
	}
	if !n.authorized(u.ChatID) || u.FromID != u.ChatID {
		n.log.Warn("telegram: update from foreign chat ignored", "chat", u.ChatID, "from", u.FromID)
		return
	}
	switch {
	case u.Callback != "" && u.Data == silenceButton:
		cut := n.o.Alarm.Silence()
		answer := "Rien à couper"
		if cut {
			answer = "Sirène coupée"
		}
		if err := n.bot.AnswerCallback(ctx, u.Callback, answer); err != nil && ctx.Err() == nil {
			n.log.Warn("telegram: answerCallback failed", "err", err)
		}
		if !cut {
			n.reply(ctx, u.ChatID, "ℹ️ Rien à couper : l'alarme n'est pas déclenchée.")
			return
		}
		msg := "🔕 Sirène coupée. L'alarme reste armée."
		if u.FromName != "" {
			msg = "🔕 Sirène coupée par " + u.FromName + ". L'alarme reste armée."
		}
		for _, id := range n.chats {
			n.reply(ctx, id, msg)
		}
	case u.Callback != "" && u.Data == armCancel:
		n.answer(ctx, u.Callback, "Annulé")
		n.reply(ctx, u.ChatID, "Armement annulé.")
	case u.Callback != "" && (strings.HasPrefix(u.Data, armForce) || strings.HasPrefix(u.Data, armNightForce)):
		n.armForced(ctx, u)
	case u.Callback != "":
		n.answer(ctx, u.Callback, "")
	case u.Text != "":
		n.command(ctx, u)
	}
}

func (n *Notifier) answer(ctx context.Context, callback, text string) {
	if err := n.bot.AnswerCallback(ctx, callback, text); err != nil && ctx.Err() == nil {
		n.log.Warn("telegram: answerCallback failed", "err", err)
	}
}

// command runs a text message or reply-keyboard press. The text is never
// logged.
func (n *Notifier) command(ctx context.Context, u Update) {
	cmd := strings.ToLower(strings.TrimSpace(u.Text))
	if strings.HasPrefix(cmd, "/") {
		cmd, _, _ = strings.Cut(cmd, " ")
		cmd, _, _ = strings.Cut(cmd, "@")
	}
	switch cmd {
	case strings.ToLower(btnArm), "/armer":
		n.arm(ctx, u, false)
	case strings.ToLower(btnNight), "/nuit":
		n.arm(ctx, u, true)
	case strings.ToLower(btnStatus), "/etat":
		n.status(ctx, u.ChatID)
	case strings.ToLower(btnPhoto), "/photo":
		n.photo(ctx, u.ChatID)
	case strings.ToLower(btnVideo), "/video":
		n.video(ctx, u.ChatID)
	case "/desarmer":
		n.reply(ctx, u.ChatID, "🔐 Le désarmement se fait uniquement au clavier.")
	default:
		n.replyKeyboard(ctx, u.ChatID, "✅ Alarme connectée.")
	}
}

// photo sends a snapshot to the requester only, without blocking polling.
func (n *Notifier) photo(ctx context.Context, chat int64) {
	if n.o.Snapshot == nil {
		n.reply(ctx, chat, msgPhotoFailed)
		return
	}
	caption := "📷 " + n.o.Now().In(alertZone).Format("15:04:05")
	n.spawn(func() {
		sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		jpeg, err := n.o.Snapshot(sctx)
		cancel()
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
			err = n.bot.SendPhoto(pctx, chat, jpeg, caption)
			cancel()
		}
		if err != nil && ctx.Err() == nil {
			n.log.Warn("telegram: photo not sent", "err", err)
			n.reply(ctx, chat, msgPhotoFailed)
		}
	})
}

// video films the next 15 s (or joins the recording in progress) and sends
// it to the requester only, without blocking polling.
func (n *Notifier) video(ctx context.Context, chat int64) {
	if n.o.Clip == nil {
		n.reply(ctx, chat, msgVideoFailed)
		return
	}
	caption := "🎥 " + n.o.Now().In(alertZone).Format("15:04:05")
	rec := n.record(ctx)
	n.reply(ctx, chat, "🎥 Enregistrement de 15 s…")
	n.spawn(func() {
		c, ok := rec.wait(ctx)
		if ctx.Err() != nil {
			return
		}
		if ok {
			err := n.bot.SendVideo(ctx, chat, c.MP4, caption, c.Width, c.Height, c.Duration)
			if err == nil || ctx.Err() != nil {
				return
			}
			n.log.Warn("telegram: video not sent", "err", err)
		}
		n.reply(ctx, chat, msgVideoFailed)
	})
}

func modeName(night bool) string {
	if night {
		return "nuit"
	}
	return "total"
}

// arm arms at once, or first warns the presser about sensors in alarm.
func (n *Notifier) arm(ctx context.Context, u Update, night bool) {
	if open := n.o.Alarm.InAlarm(night); len(open) > 0 {
		n.warnOpen(ctx, u.ChatID, night, open)
		return
	}
	n.armNow(ctx, u, night, false)
}

// warnOpen asks the presser (only) whether to arm despite sensors in alarm.
// The ids ride in the button data (64 bytes at most): the ones that do not
// fit are left out and count as new when the confirmation is rechecked.
func (n *Notifier) warnOpen(ctx context.Context, chat int64, night bool, open []int) {
	prefix := armForce
	if night {
		prefix = armNightForce
	}
	names := make([]string, len(open))
	data, full := prefix, false
	for i, id := range open {
		names[i] = n.o.Name(id)
		if full {
			continue
		}
		next := data + strconv.Itoa(id)
		if data != prefix {
			next = data + "," + strconv.Itoa(id)
		}
		if len(next) > maxCallback {
			full = true
			continue
		}
		data = next
	}
	text := "⚠️ " + strings.Join(names, ", ") + " : en alerte (ouvert ou mouvement). Armer quand même ? Ces capteurs seront ignorés jusqu'à leur retour au repos."
	err := n.bot.SendButtons(ctx, chat, text, []Button{{Text: "Armer quand même", Data: data}, {Text: "Annuler", Data: armCancel}})
	if err != nil && ctx.Err() == nil {
		n.log.Warn("telegram: reply failed", "err", err)
	}
}

// armForced handles "arm anyway": it rechecks the sensors in alarm and
// warns again if one is not in the list the user was shown.
func (n *Notifier) armForced(ctx context.Context, u Update) {
	night := strings.HasPrefix(u.Data, armNightForce)
	list := strings.TrimPrefix(strings.TrimPrefix(u.Data, armNightForce), armForce)
	warned := map[int]bool{}
	for _, f := range strings.Split(list, ",") {
		id, err := strconv.Atoi(f)
		if err != nil || id < 0 {
			n.answer(ctx, u.Callback, "")
			return
		}
		warned[id] = true
	}
	n.answer(ctx, u.Callback, "OK")
	open := n.o.Alarm.InAlarm(night)
	for _, id := range open {
		if !warned[id] {
			n.warnOpen(ctx, u.ChatID, night, open)
			return
		}
	}
	n.armNow(ctx, u, night, true)
}

func (n *Notifier) armNow(ctx context.Context, u Update, night, ignoreOpen bool) {
	res := n.o.Alarm.ArmRemote(night, ignoreOpen)
	mode := modeName(night)
	switch res {
	case alarm.ArmDone, alarm.ArmSwitched:
		icon := "🔒"
		if night {
			icon = "🌙"
		}
		msg := icon + " Alarme armée (" + mode + ")"
		if u.FromName != "" {
			msg += " par " + u.FromName
		}
		msg += "."
		for _, id := range n.chats {
			n.reply(ctx, id, msg)
		}
	case alarm.ArmAlready:
		n.reply(ctx, u.ChatID, "ℹ️ L'alarme est déjà armée ("+mode+").")
	default:
		state := "occupée"
		switch s := n.o.Alarm.Snapshot().State; s {
		case alarm.StateArming:
			state = "en cours d'armement"
		case alarm.StatePending:
			state = "en délai d'entrée"
		case alarm.StateTriggered:
			state = "déclenchée"
		}
		n.reply(ctx, u.ChatID, "⚠️ Impossible d'armer maintenant : l'alarme est "+state+".")
	}
}

func (n *Notifier) status(ctx context.Context, chat int64) {
	snap := n.o.Alarm.Snapshot()
	since := ""
	if snap.ArmedAt != 0 {
		since = " depuis " + time.Unix(snap.ArmedAt, 0).In(alertZone).Format("15:04")
	}
	var head string
	switch snap.State {
	case alarm.StateDisarmed:
		head = "ℹ️ Désarmée."
	case alarm.StateArmedAway:
		head = "🔒 Armée (total)" + since + "."
	case alarm.StateArmedNight:
		head = "🌙 Armée (nuit)" + since + "."
	case alarm.StateArming:
		head = "⏳ Armement en cours."
	case alarm.StatePending:
		head = "⏳ Délai d'entrée en cours."
	case alarm.StateTriggered:
		head = "🚨 Alarme déclenchée !"
	default:
		head = "ℹ️ État : " + string(snap.State) + "."
	}
	second := "Tout est fermé."
	if open := n.o.Alarm.InAlarm(false); len(open) > 0 {
		names := make([]string, len(open))
		for i, id := range open {
			names[i] = n.o.Name(id)
		}
		second = "En alerte : " + strings.Join(names, ", ") + "."
	}
	n.replyKeyboard(ctx, chat, head+"\n"+second)
}

func (n *Notifier) replyKeyboard(ctx context.Context, chat int64, text string) {
	if err := n.bot.SendKeyboard(ctx, chat, text, mainKeyboard); err != nil && ctx.Err() == nil {
		n.log.Warn("telegram: reply failed", "err", err)
	}
}

func (n *Notifier) reply(ctx context.Context, chat int64, text string) {
	if err := n.bot.SendMessage(ctx, chat, text, nil); err != nil && ctx.Err() == nil {
		n.log.Warn("telegram: reply failed", "err", err)
	}
}
