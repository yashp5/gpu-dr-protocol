package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/grene-labs/grene/enve"
	"github.com/grene-labs/grene/http/writeresp"
	"github.com/grene-labs/grene/logger"
	"github.com/grene-labs/grene/ocp"
)

// AgentKey identifies an agent connection scope.
// If GPUIndex and GPUUUID are both zero-values, the key targets the whole node.
type AgentKey struct {
	Node     string
	GPUIndex *int
	GPUUUID  string
}

type client struct {
	key     AgentKey
	conn    *websocket.Conn
	send    chan ocp.OutgoingFrame
	pending sync.Map
	log     *slog.Logger
}

type pendingRequest struct {
	action string
	sentAt time.Time
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

// PublishCall enqueues an ocp CALL message to all clients matching the filter.
// Returns (queued, matched).
func (h *hub) publishCall(filter AgentKey, frame ocp.OutgoingFrame) (int, int, error) {
	if frame.Type != ocp.MessageTypeCall {
		return 0, 0, fmt.Errorf("ws: publishCall requires CALL frame (got %d)", frame.Type)
	}
	if frame.UniqueID == "" {
		return 0, 0, fmt.Errorf("ws: publishCall missing unique id")
	}
	if frame.Action == "" {
		return 0, 0, fmt.Errorf("ws: publishCall missing action")
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	matched := 0
	queued := 0
	for c := range h.clients {
		if !matches(filter, c.key) {
			continue
		}
		matched++
		if c.enqueueFrame(frame) {
			queued++
		}
	}
	return queued, matched, nil
}

var defaultHub = newHub()

// PublishCall is the package-level helper that uses the default hub.
func PublishCall(filter AgentKey, id string, action string, payload any) (queued int, matched int, err error) {
	frame := ocp.OutgoingFrame{
		Type:     ocp.MessageTypeCall,
		UniqueID: id,
		Action:   action,
		Payload:  payload,
	}
	return defaultHub.publishCall(filter, frame)
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
			send: make(chan ocp.OutgoingFrame, 32),
			log:  log,
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
			tick := time.NewTicker(enve.DurationOr("DORA_STREAM_PING_TICK", 30*time.Second))
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if err := c.Ping(ctx); err != nil {
						log.Error("ping: error", "err", err.Error())
						c.Close(websocket.StatusInternalError, "ping error")
						return
					}
				}
			}
		}()

		// reader: accept inbound ocp frames from agent
		go func() {
			for {
				frame, err := ocp.ReadFrame(ctx, c)
				if err != nil {
					cancel()
					return
				}
				switch frame.Type {
				case ocp.MessageTypeCall:
					handleAgentCall(cl, frame)
				case ocp.MessageTypeCallResult:
					cl.handleCallResult(frame)
				case ocp.MessageTypeCallError:
					cl.handleCallError(frame)
				default:
					cl.log.Warn("unknown frame type", "type", frame.Type)
				}
			}
		}()

		// writer: send frames to agent
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-cl.send:
					if msg.Type == ocp.MessageTypeCall && msg.UniqueID != "" {
						cl.pending.Store(msg.UniqueID, pendingRequest{action: msg.Action, sentAt: time.Now().UTC()})
					}
					if err := ocp.WriteFrame(ctx, c, msg); err != nil {
						cl.log.Error("write frame", "err", err)
						if msg.Type == ocp.MessageTypeCall && msg.UniqueID != "" {
							cl.pending.Delete(msg.UniqueID)
						}
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

func (c *client) enqueueFrame(frame ocp.OutgoingFrame) bool {
	select {
	case c.send <- frame:
		return true
	default:
		c.log.Warn("dropping outgoing frame", "id", frame.UniqueID, "action", frame.Action, "type", frame.Type)
		return false
	}
}

func handleAgentCall(cl *client, frame ocp.IncomingFrame) {
	switch frame.Action {
	case ocp.ActionBootNotification:
		var req ocp.BootNotificationRequest
		if err := json.Unmarshal(frame.Payload, &req); err != nil {
			cl.sendCallError(frame.UniqueID, ocp.ErrorCodeFormatViolation, "invalid BootNotification payload", map[string]any{"error": err.Error()})
			return
		}
		if req.Node != "" && req.Node != cl.key.Node {
			cl.log.Warn("boot notification node mismatch", "expected", cl.key.Node, "reported", req.Node)
		}
		cl.log.Info("boot notification", "node", cl.key.Node, "agent_version", req.AgentVersion, "vendor", req.Vendor, "model", req.Model, "capabilities", req.Capabilities)
		resp := ocp.BootNotificationResponse{
			Status:      "Accepted",
			CurrentTime: time.Now().UTC(),
			Interval:    60,
		}
		cl.sendCallResult(frame.UniqueID, resp)
	case ocp.ActionHeartbeat:
		cl.sendCallResult(frame.UniqueID, ocp.HeartbeatResponse{CurrentTime: time.Now().UTC()})
		cl.log.Debug("heartbeat", "node", cl.key.Node)
	case ocp.ActionStatusNotification:
		var req ocp.StatusNotificationRequest
		if err := json.Unmarshal(frame.Payload, &req); err != nil {
			cl.sendCallError(frame.UniqueID, ocp.ErrorCodeFormatViolation, "invalid StatusNotification payload", map[string]any{"error": err.Error()})
			return
		}
		cl.log.Info("status notification", "node", req.Node, "gpu_index", req.GPU.Index, "gpu_uuid", req.GPU.UUID, "status", req.Status, "event_id", req.EventID, "timestamp", req.Timestamp)
		cl.sendCallResult(frame.UniqueID, ocp.StatusNotificationResponse{})
	case ocp.ActionMeterValues:
		var req ocp.MeterValuesRequest
		if err := json.Unmarshal(frame.Payload, &req); err != nil {
			cl.sendCallError(frame.UniqueID, ocp.ErrorCodeFormatViolation, "invalid MeterValues payload", map[string]any{"error": err.Error()})
			return
		}
		cl.log.Info(
			"meter values",
			"node", req.Node,
			"gpu_index", req.GPU.Index,
			"gpu_uuid", req.GPU.UUID,
			"power_w", req.PowerW,
			"cap_w", req.PowerCapW,
			"util", req.UtilPercent,
			"temp_c", req.TempC,
			"event_id", req.EventID,
			"event_state", req.EventState,
			"timestamp", req.TS,
		)
		cl.sendCallResult(frame.UniqueID, ocp.MeterValuesResponse{})
	default:
		cl.log.Warn("unsupported agent action", "action", frame.Action)
		cl.sendCallError(frame.UniqueID, ocp.ErrorCodeNotImplemented, fmt.Sprintf("unsupported action %q", frame.Action), map[string]any{"action": frame.Action})
	}
}

func (c *client) sendCallResult(uniqueID string, payload any) {
	frame := ocp.OutgoingFrame{
		Type:     ocp.MessageTypeCallResult,
		UniqueID: uniqueID,
		Payload:  payload,
	}
	if !c.enqueueFrame(frame) {
		c.log.Warn("failed to queue call result", "id", uniqueID)
	}
}

func (c *client) sendCallError(uniqueID string, code ocp.ErrorCode, description string, details any) {
	frame := ocp.OutgoingFrame{
		Type:             ocp.MessageTypeCallError,
		UniqueID:         uniqueID,
		ErrorCode:        code,
		ErrorDescription: description,
		ErrorDetails:     details,
	}
	if !c.enqueueFrame(frame) {
		c.log.Warn("failed to queue call error", "id", uniqueID, "code", code)
	}
}

func (c *client) handleCallResult(frame ocp.IncomingFrame) {
	value, ok := c.pending.LoadAndDelete(frame.UniqueID)
	if !ok {
		c.log.Warn("unexpected call result", "id", frame.UniqueID)
		return
	}
	req := value.(pendingRequest)
	latency := time.Since(req.sentAt)
	switch req.action {
	case ocp.ActionSetCurtailmentProfile:
		var resp ocp.SetCurtailmentProfileResponse
		if len(frame.Payload) != 0 {
			if err := json.Unmarshal(frame.Payload, &resp); err != nil {
				c.log.Warn("invalid SetCurtailmentProfile response", "id", frame.UniqueID, "err", err)
				return
			}
		}
		c.log.Info("curtailment acknowledged", "id", frame.UniqueID, "status", resp.Status, "duration", latency)
	default:
		c.log.Info("call acknowledged", "id", frame.UniqueID, "action", req.action, "duration", latency)
	}
}

func (c *client) handleCallError(frame ocp.IncomingFrame) {
	value, ok := c.pending.LoadAndDelete(frame.UniqueID)
	details := string(frame.ErrorDetails)
	if !ok {
		c.log.Error("call error", "id", frame.UniqueID, "code", frame.ErrorCode, "description", frame.ErrorDescription, "details", details)
		return
	}
	req := value.(pendingRequest)
	latency := time.Since(req.sentAt)
	c.log.Error("call error", "id", frame.UniqueID, "action", req.action, "code", frame.ErrorCode, "description", frame.ErrorDescription, "duration", latency, "details", details)
}
