package ws

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/grene-labs/grene/enve"
	"github.com/grene-labs/grene/http/writeresp"
	"github.com/grene-labs/grene/logger"
)

// AgentKey identifies an agent connection scope.
// If GPUIndex and GPUUUID are both zero-values, the key targets the whole node.
type AgentKey struct {
	Node     string
	GPUIndex *int
	GPUUUID  string
}

type client struct {
	key  AgentKey
	conn *websocket.Conn
	send chan any
}

type hub struct {
	mu           sync.RWMutex
	clients      map[*client]struct{}
	nodeToClient map[string]*client
}

func newHub() *hub {
	return &hub{clients: make(map[*client]struct{}), nodeToClient: make(map[string]*client)}
}

func (h *hub) register(c *client) {
	h.mu.Lock()
	// Enforce single websocket per node: evict previous if exists
	if prev := h.nodeToClient[c.key.Node]; prev != nil && prev != c {
		delete(h.clients, prev)
		_ = prev.conn.Close(websocket.StatusNormalClosure, "replaced by new connection")
	}
	h.clients[c] = struct{}{}
	h.nodeToClient[c.key.Node] = c
	h.mu.Unlock()
}

func (h *hub) unregister(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	if h.nodeToClient[c.key.Node] == c {
		delete(h.nodeToClient, c.key.Node)
	}
	h.mu.Unlock()
}

func matches(filter AgentKey, k AgentKey) bool {
	if filter.Node != "" && k.Node != filter.Node {
		return false
	}
	// If client key pins to a GPU, require equality; otherwise client is wildcard for node
	if k.GPUIndex != nil && filter.GPUIndex != nil && *k.GPUIndex != *filter.GPUIndex {
		return false
	}
	if k.GPUUUID != "" && filter.GPUUUID != "" && k.GPUUUID != filter.GPUUUID {
		return false
	}
	return true
}

// Publish enqueues a message to all clients matching the filter.
// Returns (queued, matched).
func (h *hub) publish(filter AgentKey, v any) (int, int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	matched := 0
	queued := 0
	for c := range h.clients {
		if !matches(filter, c.key) {
			continue
		}
		matched++
		select {
		case c.send <- v:
			queued++
		default:
			// drop if the client is slow; writer loop will keep up or connection will close
		}
	}
	return queued, matched
}

var defaultHub = newHub()

// Publish is the package-level helper that uses the default hub.
func Publish(filter AgentKey, v any) (queued int, matched int) {
	return defaultHub.publish(filter, v)
}

func Subscribe() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log := logger.FromCtx(r.Context()).WithGroup("websocket")
		q := r.URL.Query()
		node := q.Get("node")
		if node == "" {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("missing required query param 'node'"), http.StatusBadRequest)
			return
		}
		// One websocket per node; the agent will route per-GPU based on message payload
		var gpuIndexPtr *int
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode:    websocket.CompressionContextTakeover,
			InsecureSkipVerify: true,
		})
		if err != nil {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("websocket accept: %w", err), http.StatusInternalServerError)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "connection closed")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cl := &client{
			key:  AgentKey{Node: node, GPUIndex: gpuIndexPtr, GPUUUID: ""},
			conn: c,
			send: make(chan any, 32),
		}
		defaultHub.register(cl)
		log.Info("agent subscribed", "node", cl.key.Node, "gpu_index", func() any {
			if cl.key.GPUIndex != nil {
				return *cl.key.GPUIndex
			}
			return nil
		}(), "gpu_uuid", cl.key.GPUUUID)

		// ping pong
		go func() {
			tick := time.Tick(enve.DurationOr("DORA_STREAM_PING_TICK", 30*time.Second))
			for range tick {
				err := c.Ping(ctx)
				if err != nil {
					log.Error("ping: error", "err", err.Error())
					c.Close(websocket.StatusInternalError, "ping error")
					return
				}
			}
		}()

		// reader: drain and ignore any incoming messages from agent
		go func() {
			for {
				var msg any
				if err := wsjson.Read(ctx, c, &msg); err != nil {
					cancel()
					return
				}
			}
		}()

		// writer: send commands to agent
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-cl.send:
					if err := wsjson.Write(ctx, c, msg); err != nil {
						cancel()
						return
					}
				}
			}
		}()

		// wait until context cancelled
		<-ctx.Done()
		defaultHub.unregister(cl)
		log.Info("agent unsubscribed", "node", cl.key.Node, "gpu_index", func() any {
			if cl.key.GPUIndex != nil {
				return *cl.key.GPUIndex
			}
			return nil
		}(), "gpu_uuid", cl.key.GPUUUID)
	}
}
