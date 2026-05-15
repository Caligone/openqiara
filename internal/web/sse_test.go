package web

import (
	"sync"
	"testing"
	"time"
)

// Avant fix : ce test paniquait régulièrement avec "send on closed channel"
// car Publish snapshotait clients[] sous lock puis envoyait hors lock,
// donc Unsubscribe(ch) pouvait close(ch) pendant le send.
//
// Doit tourner avec `go test -race` pour valider l'absence de data race
// sur les structures sous-jacentes.
func TestSSEHubPublishUnsubscribeRace(t *testing.T) {
	hub := newSSEHub()

	const subscribers = 20
	const events = 500

	var wg sync.WaitGroup

	// N subscribers qui s'abonnent, drainent, puis se désabonnent.
	for i := 0; i < subscribers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := hub.Subscribe()
			deadline := time.Now().Add(50 * time.Millisecond)
			for time.Now().Before(deadline) {
				select {
				case <-ch:
				default:
				}
			}
			hub.Unsubscribe(ch)
		}()
	}

	// 1 publisher qui spam des events.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < events; i++ {
			hub.Publish(sseEvent{Type: "test", Data: i})
		}
	}()

	wg.Wait()
}

// Sanity check : un client abonné reçoit bien les events publiés.
func TestSSEHubSubscribeReceives(t *testing.T) {
	hub := newSSEHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	go hub.Publish(sseEvent{Type: "alarm", Data: "armed"})

	select {
	case ev := <-ch:
		if ev.Type != "alarm" {
			t.Errorf("type = %q, want alarm", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the event")
	}
}

// Subscribe doit replayer le dernier event de chaque type aux nouveaux
// clients (sync immédiate sans attendre le prochain Publish).
func TestSSEHubSubscribeReplaysLatest(t *testing.T) {
	hub := newSSEHub()
	hub.Publish(sseEvent{Type: "alarm", Data: "armed"})
	hub.Publish(sseEvent{Type: "sensors", Data: 3})

	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	seen := map[string]bool{}
	timeout := time.After(100 * time.Millisecond)
loop:
	for len(seen) < 2 {
		select {
		case ev := <-ch:
			seen[ev.Type] = true
		case <-timeout:
			break loop
		}
	}
	if !seen["alarm"] || !seen["sensors"] {
		t.Errorf("subscribe did not replay both types: seen=%v", seen)
	}
}
