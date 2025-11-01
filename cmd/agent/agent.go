package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/grene-labs/grene/ocp"
)

type ocppSession struct {
	conn    *websocket.Conn
	send    chan ocp.OutgoingFrame
	pending sync.Map // map[string]*pendingCall
	log     *slog.Logger
}

type pendingCall struct {
	action string
	respCh chan ocp.IncomingFrame
}

func newOCPPSession(conn *websocket.Conn, log *slog.Logger) *ocppSession {
	return &ocppSession{
		conn: conn,
		send: make(chan ocp.OutgoingFrame, 32),
		log:  log,
	}
}

func (s *ocppSession) start(ctx context.Context, cancel context.CancelFunc) <-chan ocp.IncomingFrame {
	incoming := make(chan ocp.IncomingFrame, 8)
	go s.writerLoop(ctx, cancel)
	go s.readerLoop(ctx, cancel, incoming)
	return incoming
}

func (s *ocppSession) writerLoop(ctx context.Context, cancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-s.send:
			if err := ocp.WriteFrame(ctx, s.conn, frame); err != nil {
				s.log.Error("write frame", "err", err)
				cancel()
				return
			}
		}
	}
}

func (s *ocppSession) readerLoop(ctx context.Context, cancel context.CancelFunc, incoming chan<- ocp.IncomingFrame) {
	defer close(incoming)
	for {
		frame, err := ocp.ReadFrame(ctx, s.conn)
		if err != nil {
			cancel()
			return
		}
		switch frame.Type {
		case ocp.MessageTypeCall:
			select {
			case incoming <- frame:
			case <-ctx.Done():
				return
			}
		case ocp.MessageTypeCallResult, ocp.MessageTypeCallError:
			s.dispatchResponse(frame)
		default:
			s.log.Warn("unknown frame type", "type", frame.Type)
		}
	}
}

func (s *ocppSession) dispatchResponse(frame ocp.IncomingFrame) {
	value, ok := s.pending.LoadAndDelete(frame.UniqueID)
	if !ok {
		s.log.Warn("unexpected response", "id", frame.UniqueID, "type", frame.Type)
		return
	}
	pc := value.(*pendingCall)
	select {
	case pc.respCh <- frame:
	default:
		s.log.Warn("response channel full", "action", pc.action)
	}
}

func (s *ocppSession) call(ctx context.Context, action string, payload any) (ocp.IncomingFrame, error) {
	msgID, err := ocp.NewMessageID()
	if err != nil {
		return ocp.IncomingFrame{}, err
	}
	respCh := make(chan ocp.IncomingFrame, 1)
	s.pending.Store(msgID, &pendingCall{action: action, respCh: respCh})
	frame := ocp.OutgoingFrame{Type: ocp.MessageTypeCall, UniqueID: msgID, Action: action, Payload: payload}
	select {
	case s.send <- frame:
	case <-ctx.Done():
		s.pending.Delete(msgID)
		return ocp.IncomingFrame{}, ctx.Err()
	}
	select {
	case resp := <-respCh:
		if resp.Type == ocp.MessageTypeCallError {
			return ocp.IncomingFrame{}, fmt.Errorf("ocpp call error: %s: %s", resp.ErrorCode, resp.ErrorDescription)
		}
		return resp, nil
	case <-ctx.Done():
		s.pending.Delete(msgID)
		return ocp.IncomingFrame{}, ctx.Err()
	}
}

func (s *ocppSession) sendCallResult(uniqueID string, payload any) {
	frame := ocp.OutgoingFrame{Type: ocp.MessageTypeCallResult, UniqueID: uniqueID, Payload: payload}
	s.enqueue(frame)
}

func (s *ocppSession) sendCallError(uniqueID string, code ocp.ErrorCode, description string, details any) {
	frame := ocp.OutgoingFrame{Type: ocp.MessageTypeCallError, UniqueID: uniqueID, ErrorCode: code, ErrorDescription: description, ErrorDetails: details}
	s.enqueue(frame)
}

func (s *ocppSession) enqueue(frame ocp.OutgoingFrame) {
	select {
	case s.send <- frame:
	default:
		s.log.Warn("dropping outgoing frame", "id", frame.UniqueID, "action", frame.Action, "type", frame.Type)
	}
}

func main() {
	var (
		serverURL     string
		nodeName      string
		rampStepSec   int
		restoreOnExit bool
	)
	flag.StringVar(&serverURL, "server", "http://localhost:8080", "Control server base URL (http(s)://host:port)")
	flag.StringVar(&nodeName, "node", "node-01", "This node name (used in subscribe query)")
	flag.IntVar(&rampStepSec, "ramp-step-s", 1, "Ramp step seconds")
	flag.BoolVar(&restoreOnExit, "restore-on-exit", true, "Restore power limit on exit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	u, err := url.Parse(serverURL)
	if err != nil {
		slog.Error("invalid server URL", "err", err)
		os.Exit(1)
	}
	q := u.Query()
	if q.Get("node") == "" {
		q.Set("node", nodeName)
	}
	u.RawQuery = q.Encode()
	wsURL := u.String()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{})
	if err != nil {
		slog.Error("websocket dial failed", "url", wsURL, "err", err)
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "agent exiting")

	log := slog.Default()
	session := newOCPPSession(conn, log)
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()
	incoming := session.start(connCtx, connCancel)

	defaultIdx := 0
	lastSel := ocp.GPUSelector{Index: &defaultIdx}

	heartbeatEvery := 60 * time.Second
	bootFrame, err := session.call(connCtx, ocp.ActionBootNotification, ocp.BootNotificationRequest{
		Node:         nodeName,
		AgentVersion: agentVersion(),
		Vendor:       "grene",
		Model:        "gpu-agent",
		Capabilities: agentCapabilities(),
	})
	if err != nil {
		log.Error("boot notification failed", "err", err)
	} else {
		var bootResp ocp.BootNotificationResponse
		if err := json.Unmarshal(bootFrame.Payload, &bootResp); err != nil {
			log.Warn("boot notification response decode", "err", err)
		} else if bootResp.Interval > 0 {
			heartbeatEvery = time.Duration(bootResp.Interval) * time.Second
		}
	}

	if err := sendStatus(connCtx, session, nodeName, lastSel, ocp.AgentStateIdle, "", "agent-online"); err != nil {
		log.Warn("initial status notification failed", "err", err)
	}

	go runHeartbeat(connCtx, session, heartbeatEvery)

	var activeCancel context.CancelFunc
	var active int32

	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-tick.C:
				if atomic.LoadInt32(&active) == 0 {
					if err := sendMeterValues(connCtx, session, nodeName, lastSel, "", string(ocp.AgentStateIdle)); err != nil {
						log.Warn("idle meter values", "err", err)
					}
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			connCancel()
			return
		case frame, ok := <-incoming:
			if !ok {
				return
			}
			switch frame.Action {
			case ocp.ActionSetCurtailmentProfile:
				var cmd ocp.SetCurtailmentProfileRequest
				if err := json.Unmarshal(frame.Payload, &cmd); err != nil {
					session.sendCallError(frame.UniqueID, ocp.ErrorCodeFormatViolation, "invalid SetCurtailmentProfile payload", map[string]any{"error": err.Error()})
					continue
				}
				if cmd.Node != "" && cmd.Node != nodeName {
					session.sendCallError(frame.UniqueID, ocp.ErrorCodeProtocolError, "command targeted different node", map[string]any{"expected": nodeName, "got": cmd.Node})
					continue
				}
				if _, err := deviceArgForSelector(cmd.GPU); err != nil {
					session.sendCallError(frame.UniqueID, ocp.ErrorCodePropertyConstraintViolation, "invalid gpu selector", map[string]any{"error": err.Error()})
					continue
				}
				if activeCancel != nil {
					activeCancel()
				}
				actionCtx, cancel := context.WithCancel(connCtx)
				activeCancel = cancel
				lastSel = cmd.GPU
				atomic.StoreInt32(&active, 1)
				session.sendCallResult(frame.UniqueID, ocp.SetCurtailmentProfileResponse{Status: "Accepted"})
				if err := sendStatus(connCtx, session, nodeName, cmd.GPU, ocp.AgentStatePending, cmd.EventID, "curtailment accepted"); err != nil {
					log.Warn("status notification", "err", err)
				}
				go func(cmd ocp.SetCurtailmentProfileRequest) {
					defer cancel()
					defer atomic.StoreInt32(&active, 0)
					if err := runCurtail(actionCtx, session, nodeName, cmd, time.Duration(rampStepSec)*time.Second, restoreOnExit); err != nil {
						slog.Error("curtail error", "err", err)
						_ = sendStatus(context.Background(), session, nodeName, cmd.GPU, ocp.AgentStateSafeRestore, cmd.EventID, err.Error())
					}
					if err := sendStatus(context.Background(), session, nodeName, cmd.GPU, ocp.AgentStateIdle, cmd.EventID, "curtailment complete"); err != nil {
						slog.Warn("status notification", "err", err)
					}
				}(cmd)
			case ocp.ActionClearCurtailmentProfile:
				session.sendCallError(frame.UniqueID, ocp.ErrorCodeNotImplemented, "ClearCurtailmentProfile not supported", nil)
			default:
				session.sendCallError(frame.UniqueID, ocp.ErrorCodeNotImplemented, fmt.Sprintf("unsupported action %q", frame.Action), map[string]any{"action": frame.Action})
			}
		}
	}
}

func agentCapabilities() []string {
	return []string{ocp.ActionSetCurtailmentProfile, ocp.ActionMeterValues, ocp.ActionStatusNotification, ocp.ActionHeartbeat}
}

func agentVersion() string {
	if v := os.Getenv("GRENE_AGENT_VERSION"); v != "" {
		return v
	}
	return "grene-agent"
}

func runHeartbeat(ctx context.Context, session *ocppSession, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := session.call(ctx, ocp.ActionHeartbeat, ocp.HeartbeatRequest{}); err != nil {
				session.log.Warn("heartbeat failed", "err", err)
			}
		}
	}
}

func sendStatus(ctx context.Context, session *ocppSession, node string, sel ocp.GPUSelector, state ocp.AgentState, eventID, info string) error {
	payload := ocp.StatusNotificationRequest{
		Node:      node,
		GPU:       sel,
		Status:    state,
		Timestamp: time.Now().UTC(),
		EventID:   eventID,
		Info:      info,
	}
	_, err := session.call(ctx, ocp.ActionStatusNotification, payload)
	return err
}

func sendMeterValues(ctx context.Context, session *ocppSession, node string, sel ocp.GPUSelector, eventID, eventState string) error {
	deviceArg, err := deviceArgForSelector(sel)
	if err != nil {
		return err
	}
	powerW, capW, util, temp, err := queryTelemetry(ctx, deviceArg)
	if err != nil {
		return err
	}
	payload := ocp.MeterValuesRequest{
		Node:        node,
		GPU:         sel,
		TS:          time.Now().UTC(),
		PowerW:      powerW,
		PowerCapW:   capW,
		UtilPercent: util,
		TempC:       temp,
		EventID:     eventID,
		EventState:  eventState,
	}
	_, err = session.call(ctx, ocp.ActionMeterValues, payload)
	return err
}

type powerLimits struct {
	MinW     int
	MaxW     int
	CurrentW int
}

func runCurtail(ctx context.Context, session *ocppSession, node string, cmd ocp.SetCurtailmentProfileRequest, step time.Duration, restoreOnExit bool) error {
	deviceArg, err := deviceArgForSelector(cmd.GPU)
	if err != nil {
		return err
	}

	pl, err := queryPowerLimits(ctx, deviceArg)
	if err != nil {
		return fmt.Errorf("query power limits: %w", err)
	}

	target := cmd.TargetCapW
	if target < pl.MinW {
		target = pl.MinW
	}
	if target > pl.MaxW {
		target = pl.MaxW
	}

	prev := pl.CurrentW
	restored := false
	restore := func() {
		if restored {
			return
		}
		_ = setPowerLimit(context.Background(), deviceArg, prev)
		restored = true
	}
	if restoreOnExit {
		defer restore()
	}

	if !cmd.StartAt.IsZero() {
		if wait := time.Until(cmd.StartAt); wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}

	if err := sendStatus(ctx, session, node, cmd.GPU, ocp.AgentStateRamping, cmd.EventID, ""); err != nil {
		slog.Warn("status notification", "state", ocp.AgentStateRamping, "err", err)
	}

	rampDur := time.Duration(cmd.RampS) * time.Second
	if rampDur <= 0 {
		if err := setPowerLimit(ctx, deviceArg, target); err != nil {
			return err
		}
	} else {
		if err := rampPower(ctx, deviceArg, pl.CurrentW, target, rampDur, step); err != nil {
			return err
		}
	}

	if err := sendStatus(ctx, session, node, cmd.GPU, ocp.AgentStateHolding, cmd.EventID, ""); err != nil {
		slog.Warn("status notification", "state", ocp.AgentStateHolding, "err", err)
	}

	if !cmd.EndAt.IsZero() {
		if wait := time.Until(cmd.EndAt); wait > 0 {
			telemTick := time.NewTicker(2 * time.Second)
			defer telemTick.Stop()
			timer := time.NewTimer(wait)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					goto doneHold
				case <-telemTick.C:
					if err := sendMeterValues(ctx, session, node, cmd.GPU, cmd.EventID, "ACTIVE"); err != nil {
						slog.Warn("meter values", "err", err)
					}
				}
			}
		}
	}

doneHold:
	if restoreOnExit {
		if err := sendStatus(ctx, session, node, cmd.GPU, ocp.AgentStateRestoring, cmd.EventID, ""); err != nil {
			slog.Warn("status notification", "state", ocp.AgentStateRestoring, "err", err)
		}
		restore()
	}

	return nil
}

func deviceArgForSelector(sel ocp.GPUSelector) (string, error) {
	if sel.UUID != "" {
		return sel.UUID, nil
	}
	if sel.Index != nil {
		return strconv.Itoa(*sel.Index), nil
	}
	return "0", nil
}

func queryPowerLimits(ctx context.Context, device string) (powerLimits, error) {
	args := []string{"-i", device, "--query-gpu=power.min_limit,power.max_limit,power.limit", "--format=csv,noheader,nounits"}
	out, err := exec.CommandContext(ctx, "nvidia-smi", args...).Output()
	if err != nil {
		return powerLimits{}, fmt.Errorf("nvidia-smi: %w", err)
	}
	reader := csv.NewReader(bytes.NewReader(out))
	reader.TrimLeadingSpace = true
	rec, err := reader.Read()
	if err != nil {
		return powerLimits{}, err
	}
	if len(rec) < 3 {
		return powerLimits{}, errors.New("unexpected nvidia-smi output")
	}
	minW, _ := strconv.Atoi(strings.TrimSpace(rec[0]))
	maxW, _ := strconv.Atoi(strings.TrimSpace(rec[1]))
	currW, _ := strconv.Atoi(strings.TrimSpace(rec[2]))
	return powerLimits{MinW: minW, MaxW: maxW, CurrentW: currW}, nil
}

func setPowerLimit(ctx context.Context, device string, watts int) error {
	cmd := exec.CommandContext(ctx, "nvidia-smi", "-i", device, "-pl", strconv.Itoa(watts))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set power limit failed: %v: %s", err, string(out))
	}
	return nil
}

func rampPower(ctx context.Context, device string, fromW, toW int, duration time.Duration, step time.Duration) error {
	if step <= 0 {
		step = time.Second
	}
	steps := int(duration / step)
	if steps <= 0 {
		return setPowerLimit(ctx, device, toW)
	}
	delta := float64(toW - fromW)
	for i := 1; i <= steps; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		progress := float64(i) / float64(steps)
		w := int(float64(fromW) + progress*delta)
		if err := setPowerLimit(ctx, device, w); err != nil {
			return err
		}
		time.Sleep(step)
	}
	return setPowerLimit(ctx, device, toW)
}

func queryTelemetry(ctx context.Context, device string) (powerW float64, capW int, util int, temp int, err error) {
	args := []string{"-i", device, "--query-gpu=power.draw,power.limit,utilization.gpu,temperature.gpu", "--format=csv,noheader,nounits"}
	out, e := exec.CommandContext(ctx, "nvidia-smi", args...).Output()
	if e != nil {
		err = fmt.Errorf("nvidia-smi telemetry: %w", e)
		return
	}
	reader := csv.NewReader(bytes.NewReader(out))
	reader.TrimLeadingSpace = true
	rec, e := reader.Read()
	if e != nil {
		err = e
		return
	}
	if len(rec) < 4 {
		err = errors.New("unexpected nvidia-smi telemetry output")
		return
	}
	powerW, _ = strconv.ParseFloat(strings.TrimSpace(rec[0]), 64)
	capW, _ = strconv.Atoi(strings.TrimSpace(rec[1]))
	util, _ = strconv.Atoi(strings.TrimSpace(rec[2]))
	temp, _ = strconv.Atoi(strings.TrimSpace(rec[3]))
	return
}
