// Runs the Grene server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // embed a copy of the timezone data for portability.
	"unsafe"

	"github.com/dusted-go/logging/prettylog"
	"github.com/google/uuid"

	"github.com/grene-labs/grene/ddslog"
	"github.com/grene-labs/grene/enve"
	"github.com/grene-labs/grene/loglimit"
	"github.com/grene-labs/grene/server"
)

// we're not interested in 32-bit portability.
func init() {
	if unsafe.Sizeof(int(0)) != unsafe.Sizeof(int64(0)) {
		panic("this program is only designed for 64-bit integers")
	}
}

// actually run the program.
func main() {
	// first: logging and environment setup, so we can know if anything else with the setup goes wrong.

	// we use gitlab.com/efronlicht/enve for environment management.
	os.Setenv(enve.ENVE_LOG_SLOG, "true")
	os.Setenv(enve.ENVE_LOG_SUCCESS, "true")
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Printf("GRENE: start: send SIGINT (ctrl+c) to stop")

	log.Printf("initenv: initializing environment from file %q", "env.json")

	_, f, _, _ := runtime.Caller(0)

	err := initenv(filepath.Join(filepath.Dir(f), "env.json"))
	if errors.Is(err, os.ErrNotExist) {
		log.Printf("initenv: no env file %q found, skipping", "env.json")
	} else if err != nil {
		log.Panicf("initenv: could not initialize environment from file %q: %v", "env.json", err)
	}
	enve.StringOr("GRENE_EXAMPLE_KEY", "foo")

	log := initlog()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := server.Run(ctx, log); err != nil {
		log.Error("error running server", "err", err)
		if errors.Is(err, context.Canceled) {
			log.Info("server stopped by signal")
		} else {
			log.Error("server stopped with error", "err", err)
		}
		os.Exit(1)
	} else {
		log.Info("server stopped cleanly")
	}
}

// initialize the logger and set it as the default, global logger.
func initlog() (logger *slog.Logger) {
	handlerOpts := &slog.HandlerOptions{
		AddSource: true,
		Level: enve.Or(
			func(s string) (slog.Level, error) {
				switch strings.ToLower(s) {
				case "debug":
					return slog.LevelDebug, nil
				case "info":
					return slog.LevelInfo, nil
				case "warn", "warning":
					return slog.LevelWarn, nil
				case "error":
					return slog.LevelError, nil
				default:
					return slog.LevelInfo, fmt.Errorf("invalid log level %q", s)
				}
			}, "GRENE_LOG_LEVEL", slog.LevelInfo,
		),
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.SourceKey {
				src, ok := a.Value.Any().(*slog.Source)
				if !ok {
					return a
				}
				// trim the file to the last 3 components, plus a .. if there's more than 3.
				fn := trimComponents(src.Function, 4)
				fn = strings.TrimSuffix(fn, "[...]")
				if _, after, ok := strings.Cut(fn, "GRENE-network/"); ok {
					fn = after // trim the GRENE prefix.
				}
				file := trimComponents(src.File, 3)
				if _, after, ok := strings.Cut(file, "GRENE-network/"); ok {
					file = after // trim the GRENE prefix.
				}
				a.Value = slog.StringValue(fmt.Sprintf("%s @ %s:%d", fn, file, src.Line))
			}
			switch k := a.Value.Kind(); k {
			case slog.KindTime: // make sure all times are always UITC everywhere.
				t := a.Value.Time().UTC()
				a.Value = slog.StringValue(t.Format(time.RFC3339))
				return a
			case slog.KindDuration: // turn duration into a string.
				d := a.Value.Duration()
				a.Value = slog.StringValue(d.String())
				return a
			case slog.KindAny:
				v := reflect.ValueOf(a.Value.Any())
				if v.Kind() == reflect.Slice && v.IsNil() {
					return slog.Attr{} // don't log nil slices.
				}

				err, ok := a.Value.Any().(error)
				if !ok {
					return a
				}
				if err == nil {
					return slog.Attr{} // don't log nil errors.
				}
				a.Value = slog.StringValue(err.Error())
				return a
			default:
				return a
			}
		},
	}

	var handler slog.Handler
	if apiKey := enve.StringOr("DATADOG_API_KEY", ""); apiKey != "" {
		var err error
		handler, err = ddslog.New(context.TODO(), ddslog.Config{
			APIKey: apiKey,
			Client: &http.Client{
				Timeout: enve.DurationOr("GRENE_DATADOG_TIMEOUT", 30*time.Second),
			},
			Target: ddslog.Target(enve.StringOr("GRENE_DATADOG_TARGET", string(ddslog.TargetUS3))),
			Opts:   *handlerOpts,
			Source: "staging",
			Writer: os.Stderr, // write to stderr as well as to datadog.
		})
		if err == nil {
			goto HANDLER_DONE
		}
		log.Printf("could not create datadog logger: %v, falling back to default loggers", err)
	}

	switch enve.StringOr("GRENE_LOG_FORMAT", "json") {
	default:
		log.Printf("invalid log format %q, using json", enve.StringOr("GRENE_LOG_FORMAT", "json"))
		fallthrough
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, handlerOpts)
	case "text":
		handler = slog.NewTextHandler(os.Stdout, handlerOpts)
	case "pretty":
		handler = prettylog.New(handlerOpts, prettylog.WithDestinationWriter(os.Stderr))
	}
HANDLER_DONE:
	// GRENE-3235: set up a log limiter.
	// see https://pkg.go.dev/gitlab.com/efronlicht/loglimit for all the info you could want on this.
	if enve.BoolOr("GRENE_LOG_LIMITER", true) {
		handler = loglimit.MustNew(
			loglimit.Config{
				Timing:  time.Tick(time.Minute),
				Handler: handler,
				// Mapping of log levels to the rate at which logs should be sampled after the stored tokens run out.
				// Must in the range [0, 1]. 0 means no logs are let through, 1 means all logs are let through.
				SampleRate: loglimit.SampleRateByLevel{Debug: 0.001, Info: 0.01, Warn: 0.1, Error: 0.2},

				// You're allowed this many tokens per tick per level...

				TokensPerTick: loglimit.TokensByLevel{
					Debug: 10,
					Info:  60,
					Warn:  120,
					Error: 1000,
				},
				// Which you can 'save up' for later, up to this maximum of bonus tokens.
				Burst: loglimit.TokensByLevel{
					Debug: 100,
					Info:  100,
					Warn:  100,
					Error: 100,
				},
			},
		)
	}

	logger = slog.New(handler)

	var ip string
	{
		rawIP, err := getoutboundIP()
		if err == nil {
			ip = rawIP.String()
		} else {
			ip = "unknown"
		}

	}
	hostname, _ := os.Hostname()
	user, _ := user.Current()
	instanceID := uuid.Must(uuid.NewV7())
	crashlogDir := enve.StringOr("GRENE_CRASHLOG_DIR", "./crashlog")

	_ = os.MkdirAll(crashlogDir, 0o755)
	{ // housekeeping: clean up old empty logs
		logs, _ := os.ReadDir(crashlogDir)

		var deleted []string
		for _, v := range logs {
			if v.IsDir() {
				continue
			}
			stat, err := v.Info()
			if err != nil {
				continue
			}
			if stat.Size() == 0 {
				oldLog := filepath.Join(crashlogDir, v.Name())
				if err := os.Remove(filepath.Join(crashlogDir, oldLog)); err == nil {
					deleted = append(deleted, oldLog)
				}
			}
			if len(deleted) > 0 {
				logger.Info("cleanp: deleted %d empty crashlogs", "deleted", deleted)
			}
		}
	}
	crashfile := filepath.Join(crashlogDir, instanceID.String()) + ".log"

	slog.SetDefault(logger)

	// setup crash logging
	{
		f, err := os.OpenFile(crashfile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
		if err != nil {
			logger.Error("could not open crash log file %q: %v", crashfile, err)
		} else {
			debug.SetCrashOutput(f, debug.CrashOptions{})
			logger.Info("opened crashlog at" + crashfile)
			defer f.Close()
		}
		logger.Info("on crash, debug info will be written to " + crashfile)

	}
	// TODO: init metrics, as separate from logger

	go func() { // spin up a goroutine to log memory stats.

		if !enve.BoolOr("GRENE_LOG_MEMORY_STATS", false) {
			return
		}
		var (
			memstats runtime.MemStats
			wait     = enve.DurationOr("GRENE_MEMORY_LOG_INTERVAL", 90*time.Second)
			group    []any // shared to reduce allocs
		)
		buildinfo, ok := debug.ReadBuildInfo()
		if ok {
			logger = logger.With(
				slog.Group(
					"buildinfo",
					"version", buildinfo.Main.Version,
					"path", buildinfo.Main.Path,
				),
			)
		}
		logger = logger.With(
			"env", enve.StringOr("GRENE_ENV", "dev"),
			slog.Group(
				"instance",
				"addr", enve.StringOr("GRENE_HTTP_SERVE_ADDR", ":8080"),
				"hostname", hostname,
				"id", instanceID.String(),
				"is_test", testing.Testing(),
				"name", "GRENE",
				"outbound_ip", ip,
				"pid", os.Getpid(),
				"start", time.Now(),
				"user", user.Username,
			),
		)
		lastTime := time.Now()
		for ; ; time.Sleep(wait) {
			runtime.ReadMemStats(&memstats)

			group = group[:0]
			// only format the by-size-allocations that we care about.
			for i := range memstats.BySize {
				if memstats.BySize[i].Mallocs != 0 {
					group = append(group, fmt.Sprintf("%05d", memstats.BySize[i].Size), memstats.BySize[i].Mallocs)
				}
			}
			mallocs := slog.Group("malloc_by_size", group...)
			group = group[:0]

			for i := range memstats.BySize {
				if memstats.BySize[i].Frees != 0 {
					group = append(group, fmt.Sprintf("%05d", memstats.BySize[i].Size), memstats.BySize[i].Frees)
				}
			}
			frees := slog.Group("frees_by_size", group...)

			logger.Info(
				"memory stats",
				slog.Group(
					"memstats",

					frees,
					mallocs,
					slog.Time("last_dump", lastTime),
					slog.String("alloc", fmtUnit(memstats.Alloc)),
					slog.String("sys", fmtUnit(memstats.Sys)),
					slog.String("total_alloc", fmtUnit(memstats.TotalAlloc)),
					slog.Duration("total_pause", time.Duration(memstats.PauseTotalNs)),
					slog.Int("num_gc", int(memstats.NumGC)),
					slog.String("heap_alloc", fmtUnit(memstats.HeapAlloc)),
					slog.String("heap_idle", fmtUnit(memstats.HeapIdle)),
					slog.String("heap_inuse", fmtUnit(memstats.HeapInuse)),
					slog.String("heap_sys", fmtUnit(memstats.HeapSys)),
				),
			)
			lastTime = time.Now()
		}
	}()
	return logger
}

// initenv loads environment variables from a JSON file into the program environment.
// The JSON file should be in the format {"KEY": "VALUE", ...}.
// If the environment variable is already set, it will not be overwritten.
func initenv(filename string) error {
	b, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("could not read env file %q: %w", filename, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("could not unmarshal env file %q: %w", filename, err)
	}
	var errs []error
	for k, v := range m {
		if _, ok := os.LookupEnv(k); ok {
			log.Printf("initenv: %q set in environment", k)
			continue
		}
		if err := os.Setenv(k, fmt.Sprint(v)); err != nil {
			errs = append(errs, fmt.Errorf("could not set env %q: %w", k, err))
		} else {
			log.Printf("initenv: %q set in file %q", k, filename)
		}
	}
	return errors.Join(errs...)
}

func getoutboundIP() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, fmt.Errorf("could not dial UDP: %w", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP, nil
}

func trimComponents(s string, n int) string {
	components := strings.Split(s, string(os.PathSeparator))
	if len(components) > n {
		return strings.Join(components[len(components)-n:], string(os.PathSeparator))
	}
	return s
}

var unittable = [4]struct {
	size float64
	unit string
}{
	{1024 * 1024 * 1024, "GiB"},
	{1024 * 1024, "MiB"},
	{1024, "KiB"},
	{0, "B"},
}

func fmtUnit[N interface {
	~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~int8 | ~int16 | ~int32 | ~int64 |
		~float32 | ~float64 | ~uintptr
}](n N) string {
	x := float64(n)
	abs := math.Abs(x)
	for i := range unittable {
		s, u := unittable[i].size, unittable[i].unit
		if abs > s {
			return fmt.Sprintf("%04.3f %s", x/s, u)
		}
	}
	return fmt.Sprint(x)
}
