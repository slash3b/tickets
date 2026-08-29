package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"go.uber.org/zap"

	"github.com/slash3b/tickets/pkg/events"
)

// Server-sent events for the seat map.
//
// REPLACES POLLING, WHICH IS THE POINT. Every browser used to ask for a whole
// section every two seconds — a gateway request, a catalog call and an inventory
// call each time, almost always to be told nothing had changed. That cost grew
// with the SIZE OF THE VENUE rather than with how much was happening, which is
// exactly backwards for an on-sale: the busier it gets, the more of the load is
// people being told "no change".
//
// SSE rather than WebSockets, deliberately. This is one-directional — the server
// has news, the browser has nothing to say back that is not already an ordinary
// POST. SSE is plain HTTP, so it crosses the Gateway with no protocol upgrade,
// reconnects on its own, and needs no library on either side.

// hub fans one Kafka stream out to many browsers.
//
// Each gateway replica has its own hub and its own Kafka group id, so every
// replica sees every message. A shared consumer group would split the partitions
// between replicas and a browser connected to any other one would never hear a
// thing — see the note in pkg/events.Subscribe.
type hub struct {
	mu sync.RWMutex
	// subscribers per event id. Section is filtered per-connection rather than
	// keyed here: a seat change knows its seats, and which section they belong
	// to is catalog's business, not something to duplicate into the message.
	subs map[string]map[chan []byte]struct{}
	lg   *zap.Logger
}

func newHub(lg *zap.Logger) *hub {
	return &hub{subs: map[string]map[chan []byte]struct{}{}, lg: lg}
}

func (h *hub) add(eventID string) chan []byte {
	// BUFFERED, AND THE BUFFER MATTERS. A browser on a slow connection must not
	// be able to block the Kafka consumer that serves every other browser on this
	// replica. When the buffer fills, that subscriber loses messages — and the
	// design tolerates that, because the seat map is refetched on connect and a
	// lost delta is corrected by the next one.
	ch := make(chan []byte, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[eventID] == nil {
		h.subs[eventID] = map[chan []byte]struct{}{}
	}
	h.subs[eventID][ch] = struct{}{}
	return ch
}

func (h *hub) remove(eventID string, ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.subs[eventID]; m != nil {
		delete(m, ch)
		if len(m) == 0 {
			delete(h.subs, eventID)
		}
	}
	close(ch)
}

func (h *hub) broadcast(c events.SeatChange) {
	body, err := json.Marshal(c)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[c.EventID] {
		select {
		case ch <- body:
		default:
			// Slow subscriber. Drop rather than block — see add().
		}
	}
}

// stream is GET /api/events/{id}/stream.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	if a.hub == nil {
		// No broker configured. Say so plainly so the browser falls back to
		// polling rather than sitting on a connection that will never speak.
		fail(w, http.StatusServiceUnavailable, "live updates are not available")
		return
	}
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx in front of this would buffer the stream into uselessness otherwise.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := a.hub.add(id.String())
	defer a.hub.remove(id.String(), ch)

	// A comment line immediately, so the browser's EventSource fires onopen and
	// any proxy in the middle learns this connection is alive.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case body := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", body)
			flusher.Flush()
		}
	}
}

// Broadcast hands a seat change to every browser watching that event.
//
// The binary owns the Kafka subscription and calls this; the API owns the
// fan-out. Keeping the wiring in main means this package needs no opinion about
// where changes come from, which is what lets the tests drive it directly.
func (a *API) Broadcast(c events.SeatChange) {
	if a.hub != nil {
		a.hub.broadcast(c)
	}
}
