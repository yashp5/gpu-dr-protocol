package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/grene-labs/grene/http/writeresp"
	"github.com/grene-labs/grene/ws"
)

type Priority string

const (
	PriorityDR    Priority = "DR"
	PriorityPrice Priority = "PRICE"
)

type EventState string

const (
	EventStatePending   EventState = "PENDING"
	EventStateActive    EventState = "ACTIVE"
	EventStateEnded     EventState = "ENDED"
	EventStateCancelled EventState = "CANCELLED"
)

type AgentState string

const (
	AgentStateIdle      AgentState = "IDLE"
	AgentStatePending   AgentState = "PENDING"
	AgentStateRamping   AgentState = "RAMPING"
	AgentStateHolding   AgentState = "HOLDING"
	AgentStateRestoring AgentState = "RESTORING"
	AgentStateSafeRest  AgentState = "SAFE_RESTORE"
)

// GPUSelector identifies a GPU on a node either by index or UUID.
type GPUSelector struct {
	Index *int   `json:"index,omitempty"`
	UUID  string `json:"uuid,omitempty"`
}

// TargetSpec is a per-node, per-GPU curtailment target.
type TargetSpec struct {
	Node         string      `json:"node"`
	GPUSelector  GPUSelector `json:"gpu_selector"`
	TargetCapW   int         `json:"target_cap_w"` // watts
	TargetReason string      `json:"target_reason,omitempty"`
}

// EventWindow defines start/end and ramp.
type EventWindow struct {
	StartAt     time.Time `json:"start_at"`
	EndAt       time.Time `json:"end_at"`
	RampSeconds int       `json:"ramp_s"`
}

// FanoutInfo summarizes command delivery to agents.
type FanoutInfo struct {
	Sent           int       `json:"sent"`
	Queued         int       `json:"queued"`
	MissingAgents  []string  `json:"missing_agents,omitempty"` // node or node#gpunum
	FailedAgents   []string  `json:"failed_agents,omitempty"`
	LastDispatchAt time.Time `json:"last_dispatch_at,omitempty"`
}

// MVSummarry holds measurement & verification rollups.
type MVSummary struct {
	BaselineW      float64 `json:"baseline_w"`
	ActualW        float64 `json:"actual_w"`
	DeliveredKW    float64 `json:"delivered_kw"`
	ResponseFirstS int     `json:"response_first_s"`
	ResponseFullS  int     `json:"response_full_s"`
}

// AgentEventStatus is per-agent (per GPU) live state.
type AgentEventStatus struct {
	Node            string     `json:"node"`
	GPUIndex        int        `json:"gpu_index"`
	Vendor          string     `json:"vendor,omitempty"` // "nvidia" | "amd"
	State           AgentState `json:"state"`
	LastPowerW      float64    `json:"last_power_w,omitempty"`
	CapW            int        `json:"cap_w,omitempty"`
	LastTelemetryAt time.Time  `json:"last_telemetry_at,omitempty"`
}

// ---------- Create / Modify Dispatch ----------

type CreateControlEventRequest struct {
	EventID string       `json:"event_id"`
	Window  EventWindow  `json:"window,omitempty"` // optional if using start_at/end_at directly
	StartAt time.Time    `json:"start_at"`
	EndAt   time.Time    `json:"end_at"`
	RampS   int          `json:"ramp_s"`
	Targets []TargetSpec `json:"targets"`

	Priority Priority `json:"priority,omitempty"` // DR (default) or PRICE
	Reason   string   `json:"reason,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

type CreateControlEventResponse struct {
	Status    string     `json:"status"` // "accepted"
	EventID   string     `json:"event_id"`
	AppliedTo []string   `json:"applied_to,omitempty"` // e.g., ["nodeA#0","nodeB#0"]
	Fanout    FanoutInfo `json:"fanout,omitempty"`
}

// ---------- Get Event State ----------

type GetControlEventStateResponse struct {
	EventID string             `json:"event_id"`
	State   EventState         `json:"state"`
	Window  EventWindow        `json:"window"`
	Summary MVSummary          `json:"summary,omitempty"`
	Agents  []AgentEventStatus `json:"agents,omitempty"`
}

// ---------- Handlers ----------

type dispatchCommand struct {
	Type       string      `json:"type"`
	EventID    string      `json:"event_id"`
	StartAt    time.Time   `json:"start_at"`
	EndAt      time.Time   `json:"end_at"`
	RampS      int         `json:"ramp_s"`
	TargetCapW int         `json:"target_cap_w"`
	Node       string      `json:"node"`
	GPU        GPUSelector `json:"gpu_selector"`
}

func CreateControlEvent() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateControlEventRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("invalid json: %w", err), http.StatusBadRequest)
			return
		}

		// Fill window if provided via StartAt/EndAt fields
		start := req.StartAt
		end := req.EndAt
		if start.IsZero() && !req.Window.StartAt.IsZero() {
			start = req.Window.StartAt
		}
		if end.IsZero() && !req.Window.EndAt.IsZero() {
			end = req.Window.EndAt
		}
		ramp := req.RampS
		if ramp == 0 && req.Window.RampSeconds != 0 {
			ramp = req.Window.RampSeconds
		}

		if req.EventID == "" {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("event_id is required"), http.StatusBadRequest)
			return
		}
		if len(req.Targets) == 0 {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("targets is required"), http.StatusBadRequest)
			return
		}
		if start.IsZero() || end.IsZero() || !end.After(start) {
			writeresp.JSONErr(r.Context(), w, fmt.Errorf("invalid window: start_at and end_at required; end_at must be after start_at"), http.StatusBadRequest)
			return
		}

		// Fanout to matching agents
		var applied []string
		var missing []string
		sentTotal := 0
		queuedTotal := 0
		lastDispatchAt := time.Now().UTC()
		for _, t := range req.Targets {
			label := t.Node
			if t.GPUSelector.Index != nil {
				label = fmt.Sprintf("%s#%d", t.Node, *t.GPUSelector.Index)
			}
			applied = append(applied, label)

			cmd := dispatchCommand{
				Type:       "dispatch",
				EventID:    req.EventID,
				StartAt:    start,
				EndAt:      end,
				RampS:      ramp,
				TargetCapW: t.TargetCapW,
				Node:       t.Node,
				GPU:        t.GPUSelector,
			}
			queued, matched := ws.Publish(ws.AgentKey{Node: t.Node, GPUIndex: t.GPUSelector.Index, GPUUUID: t.GPUSelector.UUID}, cmd)
			queuedTotal += queued
			sentTotal += queued
			if matched == 0 {
				missing = append(missing, label)
			}
		}

		resp := CreateControlEventResponse{
			Status:    "accepted",
			EventID:   req.EventID,
			AppliedTo: applied,
			Fanout: FanoutInfo{
				Sent:           sentTotal,
				Queued:         queuedTotal,
				MissingAgents:  missing,
				LastDispatchAt: lastDispatchAt,
			},
		}
		writeresp.JSONData(r.Context(), w, resp, http.StatusAccepted)
	}
}
