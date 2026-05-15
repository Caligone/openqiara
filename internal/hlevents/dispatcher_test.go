package hlevents

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func newTestDispatcher(t *testing.T, exitTimeout time.Duration) (*Dispatcher, *recorder) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := newRecorder()
	d := NewDispatcher(logger, rec.sink, exitTimeout)
	t.Cleanup(d.Reset)
	return d, rec
}

type recorder struct {
	mu   sync.Mutex
	got  []Detection
	cond *sync.Cond
}

func newRecorder() *recorder {
	r := &recorder{}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *recorder) sink(det Detection) {
	r.mu.Lock()
	r.got = append(r.got, det)
	r.cond.Broadcast()
	r.mu.Unlock()
}

func (r *recorder) waitN(t *testing.T, n int, timeout time.Duration) []Detection {
	t.Helper()
	deadline := time.Now().Add(timeout)
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.got) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("expected %d detections, got %d after %v", n, len(r.got), timeout)
		}
		done := make(chan struct{})
		go func() {
			time.Sleep(remaining)
			r.mu.Lock()
			r.cond.Broadcast()
			r.mu.Unlock()
			close(done)
		}()
		r.cond.Wait()
		select {
		case <-done:
		default:
		}
	}
	out := append([]Detection(nil), r.got...)
	return out
}

func TestDispatcher_HumanEnterExit(t *testing.T) {
	d, rec := newTestDispatcher(t, 30*time.Second)

	// Enter human id=26
	d.HandleNotification(context.Background(), NotificationItem{
		Timestamp: 1000,
		Notif: Notification{
			Type: NotifTypeIV,
			Data: NotificationData{
				Events:  []IVEvent{{EventType: IVEventEntered, Timestamp: 1000}},
				Objects: []IVObject{{Class: IVClassHuman, Confidence: 1, ID: "26"}},
			},
		},
	})

	dets := rec.waitN(t, 1, 100*time.Millisecond)
	if dets[0].Class != IVClassHuman || dets[0].ObjectID != "26" || !dets[0].Present {
		t.Errorf("first detection wrong: %+v", dets[0])
	}

	// Exit (sans objects, comme observé en live)
	d.HandleNotification(context.Background(), NotificationItem{
		Timestamp: 1010,
		Notif: Notification{
			Type: NotifTypeIV,
			Data: NotificationData{
				Events: []IVEvent{{EventType: IVEventExited, Timestamp: 1010}},
			},
		},
	})

	dets = rec.waitN(t, 2, 100*time.Millisecond)
	if dets[1].Present {
		t.Errorf("exit should yield Present=false: %+v", dets[1])
	}
	if dets[1].ObjectID != "26" {
		t.Errorf("exit should target id=26, got %q", dets[1].ObjectID)
	}
}

func TestDispatcher_AutoExpire(t *testing.T) {
	// Timeout court pour ne pas faire traîner les tests.
	d, rec := newTestDispatcher(t, 50*time.Millisecond)

	// Enter human, mais pas d'exit.
	d.HandleNotification(context.Background(), NotificationItem{
		Timestamp: 2000,
		Notif: Notification{
			Type: NotifTypeIV,
			Data: NotificationData{
				Events:  []IVEvent{{EventType: IVEventEntered, Timestamp: 2000}},
				Objects: []IVObject{{Class: IVClassHuman, Confidence: 1, ID: "99"}},
			},
		},
	})

	// Première Detection : Present=true
	dets := rec.waitN(t, 1, 100*time.Millisecond)
	if !dets[0].Present {
		t.Errorf("first detection should be Present=true")
	}

	// Attendre que le timer auto-expire (50ms + marge).
	dets = rec.waitN(t, 2, 500*time.Millisecond)
	if dets[1].Present {
		t.Errorf("auto-expire should yield Present=false, got %+v", dets[1])
	}
	if dets[1].ObjectID != "99" {
		t.Errorf("auto-expire wrong id: %q", dets[1].ObjectID)
	}
}

func TestDispatcher_ClassifiedUpdatesConfidence(t *testing.T) {
	d, rec := newTestDispatcher(t, 30*time.Second)

	// Enter avec confidence basse.
	d.HandleNotification(context.Background(), NotificationItem{
		Timestamp: 3000,
		Notif: Notification{
			Type: NotifTypeIV,
			Data: NotificationData{
				Events:  []IVEvent{{EventType: IVEventEntered, Timestamp: 3000}},
				Objects: []IVObject{{Class: IVClassHuman, Confidence: 0.3, ID: "42"}},
			},
		},
	})
	rec.waitN(t, 1, 100*time.Millisecond)

	// Classified avec confidence haute.
	d.HandleNotification(context.Background(), NotificationItem{
		Timestamp: 3010,
		Notif: Notification{
			Type: NotifTypeIV,
			Data: NotificationData{
				Events:  []IVEvent{{EventType: IVEventClassified, Timestamp: 3010}},
				Objects: []IVObject{{Class: IVClassHuman, Confidence: 0.99, ID: "42"}},
			},
		},
	})

	dets := rec.waitN(t, 2, 100*time.Millisecond)
	if dets[1].Confidence < 0.9 {
		t.Errorf("classified should update confidence to ~0.99, got %f", dets[1].Confidence)
	}
}

func TestDispatcher_UnknownNotifType(t *testing.T) {
	d, rec := newTestDispatcher(t, 30*time.Second)
	d.HandleNotification(context.Background(), NotificationItem{
		Timestamp: 4000,
		Notif:     Notification{Type: "unknown_type"},
	})
	// Ne doit rien produire ; donc on attend 50ms et on vérifie qu'on a 0.
	time.Sleep(50 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 0 {
		t.Errorf("unknown notif should yield no detection, got %d", len(rec.got))
	}
}
