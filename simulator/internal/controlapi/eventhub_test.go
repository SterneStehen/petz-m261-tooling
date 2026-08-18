package controlapi

import (
	"testing"
	"time"
)

func TestEventHubFutureLastEventIDRequiresResync(t *testing.T) {
	h := newEventHub(4)
	h.publish("fault", time.Time{}, nil, map[string]any{"n": 1})
	h.publish("reset", time.Time{}, nil, map[string]any{"n": 2})
	_, replay, resync, unsubscribe := h.subscribe(500)
	defer unsubscribe()
	if !resync || len(replay) != 0 {
		t.Fatalf("future Last-Event-ID: resync=%v replay=%d, want true/0", resync, len(replay))
	}
}

func TestEventHubExpiredHistoryRequiresResyncAndRecentHistoryReplays(t *testing.T) {
	h := newEventHub(2)
	for i := 0; i < 4; i++ {
		h.publish("fault", time.Time{}, nil, i)
	}
	_, _, expired, stopExpired := h.subscribe(1)
	stopExpired()
	if !expired {
		t.Fatal("expired event history did not require resync")
	}
	_, replay, recent, stopRecent := h.subscribe(3)
	defer stopRecent()
	if recent || len(replay) != 1 || replay[0].ID != 4 {
		t.Fatalf("recent replay: resync=%v replay=%+v", recent, replay)
	}
}
