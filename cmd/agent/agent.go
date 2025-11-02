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
	"math"
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
	"github.com/dusted-go/logging/prettylog"
	"github.com/grene-labs/grene/logger"
	"github.com/grene-labs/grene/ocp"
)

type ocpSession struct {
	conn    *websocket.Conn
	send    chan ocp.OutgoingFrame
	pending sync.Map // map[string]*pendingCall
	log     *slog.Logger
}

type pendingCall struct {
	action string
	respCh chan ocp.IncomingFrame
}

type GPUDriver interface {
	QueryPowerLimits(ctx context.Context, device string) (powerLimits, error)
	SetPowerLimit(ctx context.Context, device string, watts int) error
	QueryTelemetry(ctx context.Context, device string) (powerW float64, capW int, util int, temp int, err error)
}

var gpuDriver GPUDriver = nvidiaSMIDriver{}

func newOCPSession(conn *websocket.Conn, log *slog.Logger) *ocpSession {
	return &ocpSession{
		conn: conn,
		send: make(chan ocp.OutgoingFrame, 32),
		log:  log,
	}
}

func (s *ocpSession) start(ctx context.Context, cancel context.CancelFunc) <-chan ocp.IncomingFrame {
	incoming := make(chan ocp.IncomingFrame, 8)
	go s.writerLoop(ctx, cancel)
	go s.readerLoop(ctx, cancel, incoming)
	return incoming
}

func (s *ocpSession) writerLoop(ctx context.Context, cancel context.CancelFunc) {
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

func (s *ocpSession) readerLoop(ctx context.Context, cancel context.CancelFunc, incoming chan<- ocp.IncomingFrame) {
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

func (s *ocpSession) dispatchResponse(frame ocp.IncomingFrame) {
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

func (s *ocpSession) call(ctx context.Context, action string, payload any) (ocp.IncomingFrame, error) {
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
		attrs := []any{"direction", "inbound", "action", action, "id", resp.UniqueID}
		if len(resp.Payload) > 0 {
			attrs = append(attrs, "payload", json.RawMessage(resp.Payload))
		}
		if resp.Type == ocp.MessageTypeCallError {
			attrs = append(attrs, "error_code", resp.ErrorCode, "error_description", resp.ErrorDescription)
			if len(resp.ErrorDetails) > 0 {
				attrs = append(attrs, "error_details", json.RawMessage(resp.ErrorDetails))
			}
			s.log.Error("ocp call error", attrs...)
			return ocp.IncomingFrame{}, fmt.Errorf("ocp call error: %s: %s", resp.ErrorCode, resp.ErrorDescription)
		}
		s.log.Info("ocp call result", attrs...)
		return resp, nil
	case <-ctx.Done():
		s.pending.Delete(msgID)
		return ocp.IncomingFrame{}, ctx.Err()
	}
}

func (s *ocpSession) sendCallResult(uniqueID string, payload any) {
	frame := ocp.OutgoingFrame{Type: ocp.MessageTypeCallResult, UniqueID: uniqueID, Payload: payload}
	s.log.Info("ocp call result", "direction", "outbound", "id", uniqueID, "payload", payload)
	s.enqueue(frame)
}

func (s *ocpSession) sendCallError(uniqueID string, code ocp.ErrorCode, description string, details any) {
	frame := ocp.OutgoingFrame{Type: ocp.MessageTypeCallError, UniqueID: uniqueID, ErrorCode: code, ErrorDescription: description, ErrorDetails: details}
	s.log.Warn("ocp call error", "direction", "outbound", "id", uniqueID, "error_code", code, "error_description", description, "error_details", details)
	s.enqueue(frame)
}

func (s *ocpSession) enqueue(frame ocp.OutgoingFrame) {
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
		mockGPU       bool
		logFormat     string
		logLevel      string
	)
	flag.StringVar(&serverURL, "server", "http://localhost:8080", "Control server base URL (http(s)://host:port)")
	flag.StringVar(&nodeName, "node", "node-01", "This node name (used in subscribe query)")
	flag.IntVar(&rampStepSec, "ramp-step-s", 1, "Ramp step seconds")
	flag.BoolVar(&restoreOnExit, "restore-on-exit", true, "Restore power limit on exit")
	flag.BoolVar(&mockGPU, "mock-gpu", false, "Use mock GPU driver (skip nvidia-smi)")
	flag.StringVar(&logFormat, "log-format", "text", "Log format: json|text|pretty")
	flag.StringVar(&logLevel, "log-level", "info", "Log level: debug|info|warn|error")
	flag.Parse()

	if mockGPU {
		gpuDriver = newMockGPUDriver()
	} else {
		gpuDriver = nvidiaSMIDriver{}
	}

	logger, err := newLogger(logFormat, logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid logging config: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

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

	session := newOCPSession(conn, slog.Default())
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
		logger.Error("boot notification failed", "err", err)
	} else {
		var bootResp ocp.BootNotificationResponse
		if err := json.Unmarshal(bootFrame.Payload, &bootResp); err != nil {
			logger.Warn("boot notification response decode", "err", err)
		} else if bootResp.Interval > 0 {
			heartbeatEvery = time.Duration(bootResp.Interval) * time.Second
		}
	}

	if err := sendStatus(connCtx, session, nodeName, lastSel, ocp.AgentStateIdle, "", "agent-online"); err != nil {
		logger.Warn("initial status notification failed", "err", err)
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
					if err := sendTelemetry(connCtx, session, nodeName, lastSel, "", string(ocp.AgentStateIdle)); err != nil {
						logger.Warn("idle telemetry", "err", err)
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
				session.log.Info("set curtailment profile", "direction", "inbound",
					slog.Group("payload",
						"node", cmd.Node,
						"gpu_index", selectorIndex(cmd.GPU),
						"gpu_uuid", cmd.GPU.UUID,
						"event_id", cmd.EventID,
						"target_cap_w", cmd.TargetCapW,
						"ramp_s", cmd.RampS,
						"start_at", cmd.StartAt,
						"end_at", cmd.EndAt,
					),
				)
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
					logger.Warn("status notification", "err", err)
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
	return []string{ocp.ActionSetCurtailmentProfile, ocp.ActionTelemetry, ocp.ActionStatusNotification, ocp.ActionHeartbeat}
}

func agentVersion() string {
	if v := os.Getenv("GRENE_AGENT_VERSION"); v != "" {
		return v
	}
	return "grene-agent"
}

func runHeartbeat(ctx context.Context, session *ocpSession, interval time.Duration) {
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

func sendStatus(ctx context.Context, session *ocpSession, node string, sel ocp.GPUSelector, state ocp.AgentState, eventID, info string) error {
	session.log.Info("status notification", "direction", "outbound",
		slog.Group("websocket",
			"node", node,
			"gpu_index", selectorIndex(sel),
			"gpu_uuid", sel.UUID,
		),
		"state", state,
		"event_id", eventID,
		"info", info,
	)
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

func sendTelemetry(ctx context.Context, session *ocpSession, node string, sel ocp.GPUSelector, eventID, eventState string) error {
	deviceArg, err := deviceArgForSelector(sel)
	if err != nil {
		return err
	}
	powerW, capW, util, temp, err := gpuDriver.QueryTelemetry(ctx, deviceArg)
	if err != nil {
		return err
	}
	session.log.Info("telemetry", "direction", "outbound",
		slog.Group("websocket",
			"node", node,
			"gpu_index", selectorIndex(sel),
			"gpu_uuid", sel.UUID,
			"power_w", powerW,
			"cap_w", capW,
			"util", util,
			"temp_c", temp,
		),
		"event_id", eventID,
		"event_state", eventState,
	)

	payload := ocp.TelemetryRequest{
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
	_, err = session.call(ctx, ocp.ActionTelemetry, payload)
	return err
}

type powerLimits struct {
	MinW     int
	MaxW     int
	CurrentW int
}

func runCurtail(ctx context.Context, session *ocpSession, node string, cmd ocp.SetCurtailmentProfileRequest, step time.Duration, restoreOnExit bool) error {
	deviceArg, err := deviceArgForSelector(cmd.GPU)
	if err != nil {
		return err
	}

	pl, err := gpuDriver.QueryPowerLimits(ctx, deviceArg)
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
		_ = gpuDriver.SetPowerLimit(context.Background(), deviceArg, prev)
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
		if err := gpuDriver.SetPowerLimit(ctx, deviceArg, target); err != nil {
			return err
		}
	} else {
		if err := rampPower(ctx, session.log, gpuDriver, deviceArg, pl.CurrentW, target, rampDur, step); err != nil {
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
					if err := sendTelemetry(ctx, session, node, cmd.GPU, cmd.EventID, "ACTIVE"); err != nil {
						slog.Warn("telemetry", "err", err)
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

func selectorIndex(sel ocp.GPUSelector) any {
	if sel.Index == nil {
		return nil
	}
	return *sel.Index
}

func newLogger(format, level string) (*slog.Logger, error) {
	lvl, err := parseLogLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl, AddSource: true, ReplaceAttr: logger.ReplaceAttr}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	case "pretty":
		return slog.New(prettylog.New(opts, prettylog.WithDestinationWriter(os.Stderr))), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
}

func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q", level)
	}
}

func rampPower(ctx context.Context, log *slog.Logger, driver GPUDriver, device string, fromW, toW int, duration time.Duration, step time.Duration) error {
	if step <= 0 {
		step = time.Second
	}
	steps := int(duration / step)
	if steps <= 0 {
		if log != nil {
			log.Info("ramp power set", "device", device, "target_w", toW, "from_w", fromW, "step_index", 0, "step_total", 0)
		}
		return driver.SetPowerLimit(ctx, device, toW)
	}
	delta := float64(toW - fromW)
	if log != nil {
		log.Info("ramp power start", "device", device, "from_w", fromW, "to_w", toW, "duration", duration, "step", step, "steps", steps)
	}
	for i := 1; i <= steps; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		progress := float64(i) / float64(steps)
		w := int(float64(fromW) + progress*delta)
		if log != nil {
			log.Info("ramp power step", "device", device, "target_w", w, "step_index", i, "step_total", steps)
		}
		if err := driver.SetPowerLimit(ctx, device, w); err != nil {
			return err
		}
		time.Sleep(step)
	}
	if log != nil {
		log.Info("ramp power complete", "device", device, "target_w", toW)
	}
	return driver.SetPowerLimit(ctx, device, toW)
}

type nvidiaSMIDriver struct{}

func (nvidiaSMIDriver) QueryPowerLimits(ctx context.Context, device string) (powerLimits, error) {
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
	minWf, _ := strconv.ParseFloat(strings.TrimSpace(rec[0]), 64)
	maxWf, _ := strconv.ParseFloat(strings.TrimSpace(rec[1]), 64)
	currWf, _ := strconv.ParseFloat(strings.TrimSpace(rec[2]), 64)
	return powerLimits{
		MinW:     int(math.Round(minWf)),
		MaxW:     int(math.Round(maxWf)),
		CurrentW: int(math.Round(currWf)),
	}, nil
}

func (nvidiaSMIDriver) SetPowerLimit(ctx context.Context, device string, watts int) error {
	cmd := exec.CommandContext(ctx, "nvidia-smi", "-i", device, "-pl", strconv.Itoa(watts))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set power limit failed: %v: %s", err, string(out))
	}
	return nil
}

func (nvidiaSMIDriver) QueryTelemetry(ctx context.Context, device string) (powerW float64, capW int, util int, temp int, err error) {
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
	capF, errCap := strconv.ParseFloat(strings.TrimSpace(rec[1]), 64)
	if errCap != nil {
		err = errCap
		return
	}
	capW = int(math.Round(capF))
	utilF, errUtil := strconv.ParseFloat(strings.TrimSpace(rec[2]), 64)
	if errUtil != nil {
		err = errUtil
		return
	}
	util = int(math.Round(utilF))
	tempF, errTemp := strconv.ParseFloat(strings.TrimSpace(rec[3]), 64)
	if errTemp != nil {
		err = errTemp
		return
	}
	temp = int(math.Round(tempF))
	return
}

type mockGPUDriver struct {
	mu      sync.Mutex
	devices map[string]*mockGPUState
}

type mockGPUState struct {
	limits powerLimits
}

func newMockGPUDriver() GPUDriver {
	return &mockGPUDriver{devices: make(map[string]*mockGPUState)}
}

func (m *mockGPUDriver) ensure(device string) *mockGPUState {
	if st, ok := m.devices[device]; ok {
		return st
	}
	st := &mockGPUState{
		limits: powerLimits{MinW: 100, MaxW: 300, CurrentW: 260},
	}
	m.devices[device] = st
	return st
}

func (m *mockGPUDriver) QueryPowerLimits(ctx context.Context, device string) (powerLimits, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensure(device)
	return st.limits, nil
}

func (m *mockGPUDriver) SetPowerLimit(ctx context.Context, device string, watts int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensure(device)
	if watts < st.limits.MinW || watts > st.limits.MaxW {
		return fmt.Errorf("mock: power limit %d outside [%d,%d]", watts, st.limits.MinW, st.limits.MaxW)
	}
	st.limits.CurrentW = watts
	return nil
}

func (m *mockGPUDriver) QueryTelemetry(ctx context.Context, device string) (float64, int, int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensure(device)
	capW := st.limits.CurrentW
	powerW := float64(capW) * 0.85
	util := 0
	if st.limits.MaxW > 0 {
		util = int(float64(capW) * 100 / float64(st.limits.MaxW))
		if util > 100 {
			util = 100
		}
	}
	temp := 45 + util/3
	return powerW, capW, util, temp, nil
}
