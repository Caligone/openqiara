package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// sseEvent is a single server-sent event broadcast to connected clients.
type sseEvent struct {
	Type string      `json:"type"` // "alarm", "sensors", "status"
	Data interface{} `json:"data"`
}

// sseHub is the fan-out for server-sent events. Sources push events into it,
// connected clients each receive their own copy via a buffered channel.
//
// Usage:
//   hub := newSSEHub()
//   hub.Publish(sseEvent{Type: "alarm", Data: snap})   // from engine callback
//   // In HTTP handler:
//   ch := hub.Subscribe(); defer hub.Unsubscribe(ch)
//   for ev := range ch { write to ResponseWriter }
type sseHub struct {
	mu      sync.RWMutex
	clients map[chan sseEvent]struct{}
	// lastByType stores the most recent event of each type so newly-connected
	// clients receive the current state immediately (no waiting for the next update).
	lastByType map[string]sseEvent
}

func newSSEHub() *sseHub {
	return &sseHub{
		clients:    make(map[chan sseEvent]struct{}),
		lastByType: make(map[string]sseEvent),
	}
}

// Publish sends an event to all subscribed clients. Non-blocking: if a client's
// buffer is full, the event is dropped for that client (slow consumer).
func (h *sseHub) Publish(ev sseEvent) {
	h.mu.Lock()
	h.lastByType[ev.Type] = ev
	clients := make([]chan sseEvent, 0, len(h.clients))
	for ch := range h.clients {
		clients = append(clients, ch)
	}
	h.mu.Unlock()

	for _, ch := range clients {
		select {
		case ch <- ev:
		default:
			// Slow consumer — drop this event for this client.
		}
	}
}

// Subscribe registers a new client and returns its event channel.
// The channel is pre-filled with the latest event of each type (if any),
// so the client is immediately in sync with the current state.
func (h *sseHub) Subscribe() chan sseEvent {
	ch := make(chan sseEvent, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	// Replay latest state snapshots to the new client.
	for _, ev := range h.lastByType {
		select {
		case ch <- ev:
		default:
		}
	}
	h.mu.Unlock()
	return ch
}

// Unsubscribe removes a client and closes its channel.
func (h *sseHub) Unsubscribe(ch chan sseEvent) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// HandleEvents is the HTTP handler that upgrades the request to SSE.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if behind one
	w.WriteHeader(http.StatusOK)

	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	// Send an initial comment line to flush headers immediately.
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Ping every 30s to keep the connection alive through proxies.
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev.Data)
			if err != nil {
				continue
			}
			// SSE format: "event: <type>\ndata: <json>\n\n"
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// PublishEvent is a public method for other goroutines to inject events into the hub.
// Used by main.go callbacks (alarm engine, camera events).
func (s *Server) PublishEvent(eventType string, data interface{}) {
	if s.hub == nil {
		return
	}
	s.hub.Publish(sseEvent{Type: eventType, Data: data})
}
