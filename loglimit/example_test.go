package loglimit_test

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/grene-labs/grene/loglimit"
	"github.com/grene-labs/grene/loglimit/testutil"
)

// Set up a global logger with sensible defaults.
func Example() {
	// build a slog.Handler to limit log traffic
	var h *loglimit.Handler = loglimit.MustNew(

		loglimit.Config{
			Burst: loglimit.TokensByLevel{
				Debug: 100,
				Info:  100,
				Warn:  100,
				Error: 100,
			}, // 100 logs of each level can be emitted in a burst
			SampleRate: loglimit.SampleRateByLevel{
				Debug: 0.01,
				Info:  0.0,
				Warn:  0.1,
				Error: 1,
			}, // 1% of debug, 0% of info, 10% of warn, and 100% of error logs are let through after the perTick limit
			TokensPerTick: loglimit.TokensByLevel{
				Debug: 1,
				Info:  2,
				Warn:  3,
				Error: 4,
			}, // 1 debug, 2 info, 3 warn, and 4 error logs per second before sampling
			Timing: time.Tick(time.Second),
			Handler: slog.NewJSONHandler(
				os.Stdout, &slog.HandlerOptions{
					AddSource: false,
					Level:     slog.LevelDebug,
					ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
						switch a.Key {
						case "time":
							return slog.Attr{}
						default:
							return a
						}
					},
				},
			),
		},
	)

	// build a logger with the handler and set it as the global default logger
	slog.SetDefault(slog.New(h))

	// log something
	slog.Info("hello")
	slog.Info("world")
	slog.Info("this won't be logged because we're over the perTick limit")

	// Output:
	// {"level":"INFO","msg":"hello"}
	// {"level":"INFO","msg":"world"}
}

// Collect statistics about the logs that were allowed and dropped.
func ExampleStats() {
	const perTick = 2
	h := loglimit.MustNew(
		loglimit.Config{
			Timing:        time.Tick(time.Second),
			Handler:       testutil.NopHandler{},
			SampleRate:    loglimit.SampleRateByLevel{},             // no sampling after the perTick limit
			TokensPerTick: loglimit.TokensByLevel{Info: 2, Warn: 1}, // 2 info, 1 warn per tick
			Burst:         loglimit.TokensByLevel{},                 // no burst mode
		},
	)

	log := slog.New(h)
	log.Debug("0")
	log.Info("1")
	log.Info("2")
	log.Warn("3")
	log.Error("4")

	stats := h.Stats()
	fmt.Println(stats)
	// Output:
	// SRC               DEBUG INFO WARN ERROR UNKNOWN TOTAL
	// ALLOWED           0     2    1    0     0       3
	// DROPPED           1     0    0    1     0       2
	// DROPPED_BY_PARENT 0     0    0    0     0       0
	// TOTAL             1     2    1    1     0       5
	//
}
