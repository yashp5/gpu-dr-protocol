package ocp

import "time"

type MessageType int

const (
	MessageTypeCall       MessageType = 2
	MessageTypeCallResult MessageType = 3
	MessageTypeCallError  MessageType = 4
)

const (
	ActionBootNotification        = "BootNotification"
	ActionHeartbeat               = "Heartbeat"
	ActionStatusNotification      = "StatusNotification"
	ActionTelemetry               = "Telemetry"
	ActionSetCurtailmentProfile   = "SetCurtailmentProfile"
	ActionClearCurtailmentProfile = "ClearCurtailmentProfile"
)

type ErrorCode string

const (
	ErrorCodeNotImplemented                ErrorCode = "NotImplemented"
	ErrorCodeNotSupported                  ErrorCode = "NotSupported"
	ErrorCodeInternalError                 ErrorCode = "InternalError"
	ErrorCodeProtocolError                 ErrorCode = "ProtocolError"
	ErrorCodeSecurityError                 ErrorCode = "SecurityError"
	ErrorCodeFormatViolation               ErrorCode = "FormatViolation"
	ErrorCodePropertyConstraintViolation   ErrorCode = "PropertyConstraintViolation"
	ErrorCodeOccurrenceConstraintViolation ErrorCode = "OccurenceConstraintViolation"
	ErrorCodeTypeConstraintViolation       ErrorCode = "TypeConstraintViolation"
	ErrorCodeGenericError                  ErrorCode = "GenericError"
)

type AgentState string

const (
	AgentStateIdle        AgentState = "IDLE"
	AgentStatePending     AgentState = "PENDING"
	AgentStateRamping     AgentState = "RAMPING"
	AgentStateHolding     AgentState = "HOLDING"
	AgentStateRestoring   AgentState = "RESTORING"
	AgentStateSafeRestore AgentState = "SAFE_RESTORE"
)

type GPUSelector struct {
	Index *int   `json:"index,omitempty"`
	UUID  string `json:"uuid,omitempty"`
}

type BootNotificationRequest struct {
	Node         string   `json:"node"`
	AgentVersion string   `json:"agent_version,omitempty"`
	Vendor       string   `json:"vendor,omitempty"`
	Model        string   `json:"model,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type BootNotificationResponse struct {
	Status      string    `json:"status"`
	CurrentTime time.Time `json:"currentTime"`
	Interval    int       `json:"interval"`
}

type HeartbeatRequest struct{}

type HeartbeatResponse struct {
	CurrentTime time.Time `json:"currentTime"`
}

type StatusNotificationRequest struct {
	Node      string      `json:"node"`
	GPU       GPUSelector `json:"gpu"`
	Status    AgentState  `json:"status"`
	Timestamp time.Time   `json:"timestamp"`
	EventID   string      `json:"event_id,omitempty"`
	Info      string      `json:"info,omitempty"`
}

type StatusNotificationResponse struct{}

type TelemetryRequest struct {
	Node        string      `json:"node"`
	GPU         GPUSelector `json:"gpu"`
	TS          time.Time   `json:"ts"`
	PowerW      float64     `json:"power_w"`
	PowerCapW   int         `json:"power_cap_w"`
	UtilPercent int         `json:"util_percent"`
	TempC       int         `json:"temp_c"`
	EventID     string      `json:"event_id,omitempty"`
	EventState  string      `json:"event_state,omitempty"`
}

type TelemetryResponse struct{}

type SetCurtailmentProfileRequest struct {
	EventID    string      `json:"event_id"`
	StartAt    time.Time   `json:"start_at"`
	EndAt      time.Time   `json:"end_at"`
	RampS      int         `json:"ramp_s"`
	TargetCapW int         `json:"target_cap_w"`
	Node       string      `json:"node"`
	GPU        GPUSelector `json:"gpu_selector"`
}

type SetCurtailmentProfileResponse struct {
	Status     string `json:"status"`
	StatusInfo string `json:"status_info,omitempty"`
}

type ClearCurtailmentProfileRequest struct {
	EventID string      `json:"event_id"`
	Node    string      `json:"node"`
	GPU     GPUSelector `json:"gpu_selector"`
}

type ClearCurtailmentProfileResponse struct{}
