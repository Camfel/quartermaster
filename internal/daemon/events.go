package daemon

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ── Event types ──────────────────────────────────────────────────────────

// Event is a single event pushed to WebSocket subscribers.
type Event struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// StatusData is the payload for "status" events — full daemon snapshot.
type StatusData struct {
	Version            string            `json:"version"`
	StartedAt          time.Time         `json:"started_at"`
	Uptime             string            `json:"uptime"`
	LastReconcile      *time.Time        `json:"last_reconcile"`
	LastReconcileError string            `json:"last_reconcile_error,omitempty"`
	ReconcileCount     int64             `json:"reconcile_count"`
	Containers         []ContainerStatus `json:"containers"`
	Watchers           []WatcherStatus   `json:"watchers"`
	LKGHealthy         bool              `json:"lkg_healthy"`
	LKGError           string            `json:"lkg_error,omitempty"`
}

// ReconcileData is the payload for "reconcile" events.
type ReconcileData struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Count   int64  `json:"count"`
}

// HealthData is the payload for "health" events.
type HealthData struct {
	Service string `json:"service"`
	Healthy bool   `json:"healthy"`
	Type    string `json:"type"`
	Error   string `json:"error,omitempty"`
}

// ContainerData is the payload for "container" events.
type ContainerData struct {
	Action string          `json:"action"` // "created", "updated", "deleted", "running"
	Name   string          `json:"name"`
	Image  string          `json:"image"`
	PID    uint32          `json:"pid,omitempty"`
	Status ContainerStatus `json:"status"`
}

// WatcherData is the payload for "watcher" events.
type WatcherData struct {
	RepoURL  string     `json:"repo_url"`
	Branch   string     `json:"branch"`
	LastHash string     `json:"last_hash"`
	LastPoll *time.Time `json:"last_poll,omitempty"`
}

// LogData is the payload for "log" events — a single log line from a container.
type LogData struct {
	Container string `json:"container"`
	Line      string `json:"line"`
}

// ── Event Hub (pub/sub) ──────────────────────────────────────────────────

type subscriber struct {
	conn   *websocket.Conn
	events chan []byte
	done   chan struct{}
}

// EventHub manages broadcasting events to all WebSocket subscribers.
type EventHub struct {
	mu          sync.RWMutex
	subscribers map[*subscriber]struct{}
}

// NewEventHub creates a new EventHub and starts a background goroutine
// that writes events to each subscriber's WebSocket connection.
func NewEventHub() *EventHub {
	return &EventHub{
		subscribers: make(map[*subscriber]struct{}),
	}
}

// Subscribe registers a new WebSocket connection and returns a channel
// for sending events.  Caller must call Unsubscribe when the connection closes.
func (h *EventHub) Subscribe(conn *websocket.Conn) *subscriber {
	sub := &subscriber{
		conn:   conn,
		events: make(chan []byte, 64),
		done:   make(chan struct{}),
	}

	h.mu.Lock()
	h.subscribers[sub] = struct{}{}
	h.mu.Unlock()

	go h.writeLoop(sub)

	log.Printf("WebSocket subscriber connected (total: %d)", h.len())
	return sub
}

// Unsubscribe removes a subscriber and closes its resources.
func (h *EventHub) Unsubscribe(sub *subscriber) {
	h.mu.Lock()
	delete(h.subscribers, sub)
	h.mu.Unlock()

	close(sub.events) // signals writeLoop to exit
	<-sub.done        // wait for writeLoop to finish
	sub.conn.Close()
	log.Printf("WebSocket subscriber disconnected (total: %d)", h.len())
}

// Publish sends an event to all subscribers.  Non-blocking — slow
// subscribers are skipped.
func (h *EventHub) Publish(raw []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for sub := range h.subscribers {
		select {
		case sub.events <- raw:
		default:
			// Subscriber too slow; drop the event to avoid blocking
			// the daemon's reconciliation loop.
		}
	}
}

// PublishEvent marshals an Event and publishes it.
func (h *EventHub) PublishEvent(typ string, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		log.Printf("EventHub: failed to marshal %s event: %v", typ, err)
		return
	}

	ev := Event{
		Type:      typ,
		Timestamp: time.Now(),
		Data:      payload,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		log.Printf("EventHub: failed to marshal event: %v", err)
		return
	}

	h.Publish(raw)
}

func (h *EventHub) len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// writeLoop pumps events from the channel to the WebSocket connection.
func (h *EventHub) writeLoop(sub *subscriber) {
	defer close(sub.done)

	for msg := range sub.events {
		sub.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := sub.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Printf("WebSocket write error: %v", err)
			return // subscriber will be cleaned up by caller
		}
	}
}
