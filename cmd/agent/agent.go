package main

import (
	"bytes"
	"context"
	"encoding/csv"
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
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

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

	wsURL, err := buildWSURL(serverURL, nodeName)
	if err != nil {
		slog.Error("invalid server URL", "err", err)
		os.Exit(1)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{})
	if err != nil {
		slog.Error("websocket dial failed", "url", wsURL, "err", err)
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "agent exiting")

	var activeCancel context.CancelFunc
	defer func() {
		if activeCancel != nil {
			activeCancel()
		}
	}()

	for {
		var msg dispatchCommand
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			if websocket.CloseStatus(err) != -1 {
				slog.Info("websocket closed", "status", websocket.CloseStatus(err))
			} else {
				slog.Error("read failed", "err", err)
			}
			break
		}

		if msg.Type != "dispatch" {
			slog.Info("ignoring message", "type", msg.Type)
			continue
		}
		if msg.Node != "" && msg.Node != nodeName {
			slog.Info("dispatch not for this node", "target", msg.Node, "node", nodeName)
			continue
		}

		if activeCancel != nil {
			activeCancel()
		}
		actionCtx, cancel := context.WithCancel(ctx)
		activeCancel = cancel

		go func(dc dispatchCommand) {
			defer cancel()
			if err := runDispatch(actionCtx, dc, time.Duration(rampStepSec)*time.Second, restoreOnExit); err != nil {
				slog.Error("dispatch error", "err", err)
			}
		}(msg)
	}
}

type gpuSelector struct {
	Index *int   `json:"index,omitempty"`
	UUID  string `json:"uuid,omitempty"`
}

type dispatchCommand struct {
	Type       string      `json:"type"`
	EventID    string      `json:"event_id"`
	StartAt    time.Time   `json:"start_at"`
	EndAt      time.Time   `json:"end_at"`
	RampS      int         `json:"ramp_s"`
	TargetCapW int         `json:"target_cap_w"`
	Node       string      `json:"node"`
	GPU        gpuSelector `json:"gpu_selector"`
}

func buildWSURL(base, node string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// ok
	default:
		return "", fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/agent/subscribe"
	q := u.Query()
	q.Set("node", node)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type powerLimits struct {
	MinW     int
	MaxW     int
	CurrentW int
}

func runDispatch(ctx context.Context, cmd dispatchCommand, step time.Duration, restoreOnExit bool) error {
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
		until := time.Until(cmd.StartAt)
		if until > 0 {
			t := time.NewTimer(until)
			select {
			case <-ctx.Done():
				if !t.Stop() {
					<-t.C
				}
				return ctx.Err()
			case <-t.C:
			}
		}
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

	if !cmd.EndAt.IsZero() {
		until := time.Until(cmd.EndAt)
		if until > 0 {
			t := time.NewTimer(until)
			select {
			case <-ctx.Done():
				if !t.Stop() {
					<-t.C
				}
				return ctx.Err()
			case <-t.C:
			}
		}
	}

	if restoreOnExit {
		restore()
	}
	return nil
}

func deviceArgForSelector(sel gpuSelector) (string, error) {
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
