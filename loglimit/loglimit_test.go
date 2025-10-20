package loglimit

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/grene-labs/grene/loglimit/testutil"
	. "github.com/grene-labs/grene/loglimit/testutil" // LogN, TestHandler, AssertContains, AssertOmits
)

func TestNewSampler(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected a panic")
		}
	}()
	_ = NewSampler(nil, nil, SampleRateByLevel{})
}

func TestNewTokenBucket(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected a panic")
		}
	}()
	_ = NewTokenBucket(nil, nil, TokensByLevel{})
}

func newrng(t *testing.T, vals ...float64) func() float64 {
	var i int
	t.Helper()

	return func() float64 {
		t.Helper()
		defer func() { i++ }()
		if i >= len(vals) {
			return 1
		}
		return vals[i]
	}
}

func alwaysEnabled(h slog.Handler) *slog.Logger {
	ch := make(chan time.Time, 1)
	return slog.New(MustNew(Config{ch, h, SampleRateByLevel{1, 1, 1, 1}, TokensByLevel{}, TokensByLevel{}}))
}

func TestWithAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer

	log := alwaysEnabled(slog.NewTextHandler(&buf, nil))

	// we can attach attributes to a logger either at creation time or later
	withAttrs := log.With("foo", "bar")

	// once the attributes are attached, they are always present

	withMore := withAttrs.With("baz", "qux")
	withMore.Log(context.TODO(), slog.LevelInfo, "test")

	AssertContains(t, buf.String(), "foo", "bar", "baz", "qux")
	buf.Reset()
	// but it doesn't pollute the previous chain
	withAttrs.Log(context.TODO(), slog.LevelInfo, "test")
	AssertContains(t, buf.String(), "foo", "bar")
	AssertOmits(t, buf.String(), "baz", "qux")
	buf.Reset()
	// again, the original chain is unaffected
	log.Log(context.TODO(), slog.LevelInfo, "test")
	AssertOmits(t, buf.String(), "foo", "bar", "baz", "qux")
}

func TestWithGroup(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := alwaysEnabled(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	withGroup := log.WithGroup("group").With("a", "b")
	withMore := withGroup.WithGroup("group2").With("c", "d")

	withMore.Debug("test")
	AssertContains(t, buf.String(), "msg=test", "group", "group2", "a", "b", "c", "d")
	AssertOmits(t, "foo", "bar")

	buf.Reset()
	withGroup.Debug("test")
	AssertContains(t, buf.String(), "msg=test", "group", "a", "b")
	AssertOmits(t, buf.String(), "group2", "c", "d")

	buf.Reset()

	log.Debug("test")
	AssertContains(t, buf.String(), "msg=test")
}

func TestBurst(t *testing.T) {
	if testing.Short() {
		t.Skipf("SKIP %s: waits for synchronization", t.Name())
	}
	t.Parallel()

	ch := make(chan time.Time, 5)
	buf, h := TestHandler(t)
	log := slog.New(
		MustNew(
			Config{
				Timing: ch, Handler: h, TokensPerTick: TokensByLevel{
					Debug: 1,
					Warn:  1,
					Info:  1,
					Error: 1,
				},
				Burst: TokensByLevel{Debug: 2, Info: 5}, // "save up" 2 extra debug logs and 5 extra info logs
			},
		),
	)
	// five ticks should fill us up to maximum capacity
	ch <- time.Now()
	ch <- time.Now()
	ch <- time.Now()
	ch <- time.Now()
	ch <- time.Now()
	time.Sleep(100 * time.Millisecond) // wait a second to synchronize the goroutines

	for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		for i := range 10 {
			log.Log(context.TODO(), lvl, fmt.Sprintf("%s_%03d", lvl, i))
		}
	}
	for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		for i := range 10 {
			log.Log(context.TODO(), lvl, fmt.Sprintf("%s_%03d", lvl, i))
		}
	}
	got := buf.String()
	AssertContains(
		t, got,
		"DEBUG_000", "DEBUG_001", "DEBUG_002",
		"INFO_000", "INFO_001", "INFO_002", "INFO_003", "INFO_004",
		"WARN_000",
		"ERROR_000",
	)
}

func TestSamplingLevelTokenBucket(t *testing.T) {
	t.Parallel()
	t.Run(
		"bounds", func(t *testing.T) {
			assert := BuildPanicAssert(t, MustNew)
			t.Parallel()
			assert("nil timing", Config{Timing: nil, Handler: testutil.NopHandler{}})
			assert(
				"nil handler",
				Config{
					Timing:        time.Tick(time.Second),
					Handler:       nil,
					SampleRate:    SampleRateByLevel{},
					TokensPerTick: TokensByLevel{},
				},
			)
			assert(
				"negative debug",
				Config{
					Timing:        time.Tick(time.Second),
					Handler:       testutil.NopHandler{},
					SampleRate:    SampleRateByLevel{-1, 0, 0, 0},
					TokensPerTick: TokensByLevel{},
				},
			)
			assert(
				"negative info",
				Config{
					Timing:        time.Tick(time.Second),
					Handler:       testutil.NopHandler{},
					SampleRate:    SampleRateByLevel{0, -1, 0, 0},
					TokensPerTick: TokensByLevel{},
				},
			)
		},
	)
	buf, h := TestHandler(t)
	// manually inject "random" numbers
	timing := make(chan time.Time, 1)
	sh := MustNew(
		Config{
			Timing:     timing,
			Handler:    h,
			SampleRate: SampleRateByLevel{Debug: 0.1, Info: 0.2, Warn: 0.3, Error: 1},
			TokensPerTick: TokensByLevel{
				Debug: 2, Info: 1, Warn: 0, Error: 5,
			},
			Burst: TokensByLevel{},
		},
	)
	defer func() {
		if sh.Stats().IsZero() {
			t.Fatal("stats is zero")
		}
	}()

	sh.rng = newrng(
		t,
		/* free, free*/ 0.01, 0.7, 1, // DBG_000, DBG_001, DBG_002
		/*free*/ 0.9, 0.5, 0.1, 1, 1, // INFO_000, INFO_003
		0.1, 0.2, 0.9, 0.29, 1,
		/* all free */
	)

	log := slog.New(sh)
	LogN(log, slog.LevelDebug, 5)
	LogN(log, slog.LevelInfo, 5)
	LogN(log, slog.LevelWarn, 5)
	LogN(log, slog.LevelError, 5)
	AssertContains(
		t, buf.String(),
		"DEBUG_000", "DEBUG_001", "DEBUG_002",
		"INFO_000", "INFO_003",
		"WARN_001", "WARN_002", "WARN_004",
		"ERROR_000", "ERROR_001", "ERROR_002", "ERROR_003", "ERROR_004",
	)
	AssertOmits(
		t, buf.String(),
		"DEBUG_003", "DEBUG_004",
		"INFO_001", "INFO_002", "INFO_004",
		"WARN_000", "WARN_003",
		"ERROR_005", "ERROR_006", "ERROR_007", "ERROR_008", "ERROR_009",
	)
}

type testDroppingHandler struct {
	slog.Handler
	drop bool
}

func TestStatsUpdate(t *testing.T) {
	t.Parallel()
	ch := make(chan time.Time, 1)
	dh := &testDroppingHandler{Handler: testutil.NopHandler{}}
	h := MustNew(
		Config{
			Timing:        ch,
			Handler:       dh,
			SampleRate:    SampleRateByLevel{Debug: 0, Info: 0, Warn: 0, Error: 0},
			TokensPerTick: TokensByLevel{Debug: 1, Info: 1, Warn: 1, Error: 1},
		},
	)
	log := slog.New(h)

	ch <- time.Now()

	for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		log.Log(context.TODO(), lvl, "test")
		dh.drop = true
		log.Log(context.TODO(), lvl, "test")
		dh.drop = false
		log.Log(context.TODO(), lvl, "test")
	}
	time.Sleep(time.Millisecond)
	s := h.Stats()
	t.Log(s)
	switch {
	case s.Allowed.Debug != 1, s.Allowed.Info != 1, s.Allowed.Warn != 1, s.Allowed.Error != 1:
		t.Errorf("allowed: %v", s.Allowed)
	case s.Dropped.Debug != 1, s.Dropped.Info != 1, s.Dropped.Warn != 1, s.Dropped.Error != 1:
		t.Errorf("dropped: %v", s.Dropped)
	case s.DroppedByParent.Debug != 1, s.DroppedByParent.Info != 1, s.DroppedByParent.Warn != 1, s.DroppedByParent.Error != 1:
		t.Errorf("dropped by parent: %v", s.DroppedByParent)
	}
}

func (h *testDroppingHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	if h.drop {
		return false
	}
	return h.Handler.Enabled(ctx, lvl)
}
