package ocp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
)

type IncomingFrame struct {
	Type             MessageType
	UniqueID         string
	Action           string
	Payload          json.RawMessage
	ErrorCode        ErrorCode
	ErrorDescription string
	ErrorDetails     json.RawMessage
}

type OutgoingFrame struct {
	Type             MessageType
	UniqueID         string
	Action           string
	Payload          any
	ErrorCode        ErrorCode
	ErrorDescription string
	ErrorDetails     any
}

func (o OutgoingFrame) toArray() ([]any, error) {
	switch o.Type {
	case MessageTypeCall:
		if o.UniqueID == "" {
			return nil, errors.New("ocp: call frame missing unique id")
		}
		if o.Action == "" {
			return nil, errors.New("ocp: call frame missing action")
		}
		payload := o.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		return []any{int(MessageTypeCall), o.UniqueID, o.Action, payload}, nil
	case MessageTypeCallResult:
		if o.UniqueID == "" {
			return nil, errors.New("ocp: call result frame missing unique id")
		}
		payload := o.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		return []any{int(MessageTypeCallResult), o.UniqueID, payload}, nil
	case MessageTypeCallError:
		if o.UniqueID == "" {
			return nil, errors.New("ocp: call error frame missing unique id")
		}
		if o.ErrorCode == "" {
			return nil, errors.New("ocp: call error frame missing error code")
		}
		details := o.ErrorDetails
		if details == nil {
			details = map[string]any{}
		}
		desc := o.ErrorDescription
		if desc == "" {
			desc = string(o.ErrorCode)
		}
		return []any{int(MessageTypeCallError), o.UniqueID, string(o.ErrorCode), desc, details}, nil
	default:
		return nil, fmt.Errorf("ocp: unsupported outgoing message type %d", o.Type)
	}
}

func WriteFrame(ctx context.Context, conn *websocket.Conn, frame OutgoingFrame) error {
	arr, err := frame.toArray()
	if err != nil {
		return err
	}
	return wsjson.Write(ctx, conn, arr)
}

func ReadFrame(ctx context.Context, conn *websocket.Conn) (IncomingFrame, error) {
	var parts []json.RawMessage
	if err := wsjson.Read(ctx, conn, &parts); err != nil {
		return IncomingFrame{}, err
	}
	return parseParts(parts)
}

func parseParts(parts []json.RawMessage) (IncomingFrame, error) {
	if len(parts) == 0 {
		return IncomingFrame{}, errors.New("ocp: empty frame")
	}
	var msgType int
	if err := json.Unmarshal(parts[0], &msgType); err != nil {
		return IncomingFrame{}, fmt.Errorf("ocp: invalid message type: %w", err)
	}
	frameType := MessageType(msgType)
	switch frameType {
	case MessageTypeCall:
		if len(parts) != 4 {
			return IncomingFrame{}, fmt.Errorf("ocp: call frame length %d", len(parts))
		}
		var uniqueID string
		if err := json.Unmarshal(parts[1], &uniqueID); err != nil {
			return IncomingFrame{}, fmt.Errorf("ocp: invalid call unique id: %w", err)
		}
		var action string
		if err := json.Unmarshal(parts[2], &action); err != nil {
			return IncomingFrame{}, fmt.Errorf("ocp: invalid call action: %w", err)
		}
		return IncomingFrame{
			Type:     frameType,
			UniqueID: uniqueID,
			Action:   action,
			Payload:  parts[3],
		}, nil
	case MessageTypeCallResult:
		if len(parts) != 3 {
			return IncomingFrame{}, fmt.Errorf("ocp: call result frame length %d", len(parts))
		}
		var uniqueID string
		if err := json.Unmarshal(parts[1], &uniqueID); err != nil {
			return IncomingFrame{}, fmt.Errorf("ocp: invalid callresult unique id: %w", err)
		}
		payload := parts[2]
		if len(payload) == 0 {
			payload = json.RawMessage([]byte("{}"))
		}
		return IncomingFrame{
			Type:     frameType,
			UniqueID: uniqueID,
			Payload:  payload,
		}, nil
	case MessageTypeCallError:
		if len(parts) != 5 {
			return IncomingFrame{}, fmt.Errorf("ocp: call error frame length %d", len(parts))
		}
		var uniqueID string
		if err := json.Unmarshal(parts[1], &uniqueID); err != nil {
			return IncomingFrame{}, fmt.Errorf("ocp: invalid callerror unique id: %w", err)
		}
		var code string
		if err := json.Unmarshal(parts[2], &code); err != nil {
			return IncomingFrame{}, fmt.Errorf("ocp: invalid callerror code: %w", err)
		}
		var description string
		if err := json.Unmarshal(parts[3], &description); err != nil {
			return IncomingFrame{}, fmt.Errorf("ocp: invalid callerror description: %w", err)
		}
		details := parts[4]
		if len(details) == 0 {
			details = json.RawMessage([]byte("{}"))
		}
		return IncomingFrame{
			Type:             frameType,
			UniqueID:         uniqueID,
			ErrorCode:        ErrorCode(code),
			ErrorDescription: description,
			ErrorDetails:     details,
		}, nil
	default:
		return IncomingFrame{}, fmt.Errorf("ocp: unknown message type %d", frameType)
	}
}

func NewMessageID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
