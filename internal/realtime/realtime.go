// Package realtime serves loft.db .subscribe() and loft.socket over WebSockets via a generic
// in-process pub/sub keyed by (site, channel). Both consumers are tenant-scoped, so events never cross
// sites. Channels are namespaced ("db:<collection>" vs "socket:<name>") so the two never collide.
// In-process is correct for a single instance; scaling out would swap the publish source for
// Postgres LISTEN/NOTIFY without changing this layer.
package realtime

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/larsakerlund/loft/internal/web"
)

const (
	maxPayload       = 64 * 1024        // per-message cap
	maxConns         = 5000             // global concurrent-WebSocket cap (process backstop)
	perSiteMaxConns  = 500              // per-tenant cap, so one site can't exhaust the global cap
	maxInboundPerSec = 50               // per-connection inbound relay rate (loft.socket DoS guard)
	bcastBuffer      = 256              // smooths bursts so publishers never block on the hub
	pingInterval     = 30 * time.Second // reap dead/idle sockets well under the reverse proxy's 1h read timeout
	pingTimeout      = 10 * time.Second
)

type subscriber struct {
	site    string
	channel string
	out     chan []byte
}

// Hub is the in-process pub/sub. Safe for concurrent use.
type Hub struct {
	add       chan *subscriber
	remove    chan *subscriber
	bcast     chan broadcast
	connCount atomic.Int64 // live WebSocket count, for the global cap

	siteMu    sync.Mutex
	siteConns map[string]int // live count per site, for the per-tenant cap

	cancelMu sync.Mutex
	cancels  map[*subscriber]context.CancelFunc // per-conn cancels, for graceful shutdown
}

// Close terminates every live WebSocket, called on graceful shutdown so connection goroutines
// don't outlive the server's drain window.
func (h *Hub) Close() {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	for _, cancel := range h.cancels {
		cancel()
	}
}

type broadcast struct {
	site    string
	channel string
	msg     []byte
	except  *subscriber
}

// NewHub starts the hub's serialiser goroutine and returns it.
func NewHub() *Hub {
	h := &Hub{
		add:       make(chan *subscriber),
		remove:    make(chan *subscriber),
		bcast:     make(chan broadcast, bcastBuffer),
		siteConns: map[string]int{},
		cancels:   map[*subscriber]context.CancelFunc{},
	}
	go h.run()
	return h
}

// acquireSite reserves a per-tenant connection slot, returning false if the site is at its cap.
func (h *Hub) acquireSite(site string) bool {
	h.siteMu.Lock()
	defer h.siteMu.Unlock()
	if h.siteConns[site] >= perSiteMaxConns {
		return false
	}
	h.siteConns[site]++
	return true
}

func (h *Hub) releaseSite(site string) {
	h.siteMu.Lock()
	defer h.siteMu.Unlock()
	if h.siteConns[site] <= 1 {
		delete(h.siteConns, site)
		return
	}
	h.siteConns[site]--
}

// chanKey indexes subscribers by their (site, channel) so a broadcast touches only the matching
// subscribers, not every live connection.
type chanKey struct{ site, channel string }

type subIndex map[chanKey]map[*subscriber]struct{}

// run owns the subscriber index on a single goroutine, so no lock is needed and a slow client can't
// block the hub (its send is dropped if its buffer is full). Broadcast is O(matching subscribers).
// The index is created once and kept across panics: a single malformed event must not kill the only
// hub goroutine (which would silently stop all realtime delivery), so a recover relaunches the loop
// with the index intact.
func (h *Hub) run() {
	idx := subIndex{}
	h.loop(idx)
}

func (h *Hub) loop(idx subIndex) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("loftd realtime hub recovered from panic: %v", r)
			go h.loop(idx)
		}
	}()
	for {
		select {
		case s := <-h.add:
			idx.add(s)
		case s := <-h.remove:
			idx.remove(s)
		case b := <-h.bcast:
			idx.dispatch(b)
		}
	}
}

func (idx subIndex) add(s *subscriber) {
	k := chanKey{s.site, s.channel}
	set := idx[k]
	if set == nil {
		set = map[*subscriber]struct{}{}
		idx[k] = set
	}
	set[s] = struct{}{}
}

func (idx subIndex) remove(s *subscriber) {
	k := chanKey{s.site, s.channel}
	set := idx[k]
	if set == nil {
		return
	}
	delete(set, s)
	if len(set) == 0 {
		delete(idx, k)
	}
}

func (idx subIndex) dispatch(b broadcast) {
	for s := range idx[chanKey{b.site, b.channel}] {
		if s == b.except {
			continue
		}
		select {
		case s.out <- b.msg:
		default: // drop for a client that can't keep up
		}
	}
}

// sendToChannel enqueues a broadcast. It is non-blocking: realtime is best-effort, so if the hub is
// momentarily saturated the message is dropped rather than stalling the caller (a db write or a peer
// relay). The buffered bcast channel absorbs normal bursts.
func (h *Hub) sendToChannel(site, channel string, msg []byte, except *subscriber) {
	select {
	case h.bcast <- broadcast{site: site, channel: channel, msg: msg, except: except}:
	default: // hub saturated; drop (best-effort)
	}
}

// PublishDb fans a loft.db change out to subscribers of the collection (including the originator).
func (h *Hub) PublishDb(site, collection string, event any) {
	msg, err := json.Marshal(event)
	if err != nil {
		return
	}
	h.sendToChannel(site, "db:"+collection, msg, nil)
}

// SubscribeHandler is the read-only loft.db .subscribe() endpoint (/api/db/subscribe?collection=).
func (h *Hub) SubscribeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("collection")
		if c == "" {
			web.Error(w, http.StatusBadRequest, "collection required")
			return
		}
		h.serve(w, r, "db:"+c, false)
	})
}

// SocketHandler is the bidirectional loft.socket endpoint (/api/socket?channel=): a client's message
// is relayed to the other clients on the same site+channel.
func (h *Hub) SocketHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("channel")
		if c == "" {
			web.Error(w, http.StatusBadRequest, "channel required")
			return
		}
		h.serve(w, r, "socket:"+c, true)
	})
}

func (h *Hub) serve(w http.ResponseWriter, r *http.Request, channel string, bidirectional bool) {
	site := web.Site(r)

	// Global connection cap: a process backstop so a flood can't exhaust goroutines/sockets.
	if h.connCount.Add(1) > maxConns {
		h.connCount.Add(-1)
		web.Error(w, http.StatusServiceUnavailable, "too many connections")
		return
	}
	defer h.connCount.Add(-1)
	// Per-tenant cap so one site can't consume the whole global budget and deny realtime to others.
	if !h.acquireSite(site) {
		web.Error(w, http.StatusServiceUnavailable, "too many connections for this site")
		return
	}
	defer h.releaseSite(site)

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxPayload)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sub := &subscriber{site: site, channel: channel, out: make(chan []byte, 32)}
	h.add <- sub
	defer func() { h.remove <- sub }()

	// Register the cancel so Close() (graceful shutdown) can terminate this connection.
	h.cancelMu.Lock()
	h.cancels[sub] = cancel
	h.cancelMu.Unlock()
	defer func() {
		h.cancelMu.Lock()
		delete(h.cancels, sub)
		h.cancelMu.Unlock()
	}()

	go h.writeLoop(ctx, cancel, conn, sub)

	// Read loop: relay (loft.socket) or simply detect close (loft.db.subscribe). Either way, reading
	// processes control frames and surfaces disconnects. A per-connection rate limit caps how fast one
	// client can fan messages out to its peers (loft.socket DoS guard).
	var sentThisSec int
	windowStart := time.Now()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if !bidirectional {
			continue
		}
		now := time.Now()
		if now.Sub(windowStart) >= time.Second {
			windowStart = now
			sentThisSec = 0
		}
		if sentThisSec >= maxInboundPerSec {
			continue // drop frames over the per-connection rate
		}
		sentThisSec++
		h.sendToChannel(sub.site, sub.channel, data, sub)
	}
}

// writeLoop is the connection's single writer (the library allows one concurrent writer): it drains
// queued messages AND sends periodic pings, so all writes (including the heartbeat) are serialized
// here. A failed write or unanswered ping cancels the connection, which reaps idle/dead sockets.
// It runs in its own goroutine, so it recovers from any panic to avoid taking down the process.
func (h *Hub) writeLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sub *subscriber) {
	defer func() {
		if r := recover(); r != nil {
			cancel()
		}
	}()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-sub.out:
			// Bound each write: a stalled flush must not block the loop (and thus the heartbeat).
			wctx, wcancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Write(wctx, websocket.MessageText, msg)
			wcancel()
			if err != nil {
				cancel()
				return
			}
		case <-ticker.C:
			pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
}
