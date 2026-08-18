package controlapi

import (
	"sync"
	"time"
)

// eventHub carries low-volume operational events separately from telemetry.
// Its bounded history makes a disconnect explicit: a reconnect either gets
// every missed event or receives resync_required.
type eventHub struct {
	mu             sync.Mutex
	next           uint64
	history        []sseEvent
	subscribers    map[int]chan sseEvent
	nextSubscriber int
	capacity       int
}

func newEventHub(capacity int) *eventHub {
	return &eventHub{subscribers: map[int]chan sseEvent{}, capacity: capacity}
}
func (h *eventHub) publish(typ string, timestamp time.Time, revision *uint64, payload any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	e := sseEvent{ID: h.next, Type: typ, Timestamp: timestamp, Revision: revision, Payload: payload}
	h.history = append(h.history, e)
	if len(h.history) > h.capacity {
		h.history = h.history[len(h.history)-h.capacity:]
	}
	for id, ch := range h.subscribers {
		select {
		case ch <- e:
		default:
			delete(h.subscribers, id)
			close(ch)
		}
	}
}
func (h *eventHub) nextID() uint64 { h.mu.Lock(); defer h.mu.Unlock(); h.next++; return h.next }
func (h *eventHub) subscribe(last uint64) (<-chan sseEvent, []sseEvent, bool, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextSubscriber
	h.nextSubscriber++
	ch := make(chan sseEvent, 64)
	h.subscribers[id] = ch
	// A client claiming an ID after this process's newest event is just as
	// out of sync as one whose requested history has expired (for example
	// after a simulator restart, when the in-memory journal starts at 1).
	resync := last > h.next || (len(h.history) > 0 && last < h.history[0].ID-1)
	replay := []sseEvent{}
	if !resync {
		for _, e := range h.history {
			if e.ID > last {
				replay = append(replay, e)
			}
		}
	}
	return ch, replay, resync, func() {
		h.mu.Lock()
		if existing, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(existing)
		}
		h.mu.Unlock()
	}
}
