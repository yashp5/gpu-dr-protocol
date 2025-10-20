package benchmark

import (
	"context"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grene-labs/grene/loglimit"
	"github.com/grene-labs/grene/loglimit/testutil"
)

var a atomic.Int64

func BenchmarkSamplingLevelTokenBucket(b *testing.B) {
	type test struct {
		name    string
		rate    loglimit.SampleRateByLevel
		perTick loglimit.TokensByLevel
	}

	tests := []test{
		{
			name: "no limits",
			perTick: loglimit.TokensByLevel{
				Debug: math.MaxInt64,
				Info:  math.MaxInt64,
				Warn:  math.MaxInt64,
				Error: math.MaxInt64,
			},
			rate: loglimit.SampleRateByLevel{Debug: 1, Info: 1, Warn: 1, Error: 1},
		},
		{
			name:    "allow all after tokenbucket",
			rate:    loglimit.SampleRateByLevel{Debug: 1, Info: 1, Warn: 1, Error: 1},
			perTick: loglimit.TokensByLevel{Debug: 1000, Info: 1000, Warn: 1000, Error: 1000},
		},
		{
			name:    "nop",
			rate:    loglimit.SampleRateByLevel{Debug: 0, Info: 0, Warn: 0, Error: 0},
			perTick: loglimit.TokensByLevel{Debug: 0, Info: 0, Warn: 0, Error: 0},
		},

		{
			name:    "allow none after tokenbucket",
			rate:    loglimit.SampleRateByLevel{Debug: 0, Info: 0, Warn: 0, Error: 0},
			perTick: loglimit.TokensByLevel{Debug: 1000, Info: 1000, Warn: 1000, Error: 1000},
		},
		{
			name:    "allow half after tokenbucket",
			rate:    loglimit.SampleRateByLevel{Debug: 0.5, Info: 0.5, Warn: 0.5, Error: 0.5},
			perTick: loglimit.TokensByLevel{Debug: 1000, Info: 1000, Warn: 1000, Error: 1000},
		},
		{
			name:    "realistic limits",
			rate:    loglimit.SampleRateByLevel{Debug: 0.01, Info: 0.1, Warn: 0.1, Error: 1},
			perTick: loglimit.TokensByLevel{Debug: 1, Info: 10, Warn: 10, Error: 100},
		},
	}
	ordering := slices.Repeat([]slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}, 1000)
	rand.Shuffle(len(ordering), func(i, j int) { ordering[i], ordering[j] = ordering[j], ordering[i] })

	for _, handler := range []struct {
		f     func() slog.Handler
		label string
	}{
		{
			f:     func() slog.Handler { return slog.NewJSONHandler(io.Discard, nil) },
			label: "json",
		},
		{
			f:     func() slog.Handler { return slog.NewTextHandler(io.Discard, nil) },
			label: "text",
		},
		{
			f:     func() slog.Handler { return testutil.NopHandler{} },
			label: "nop",
		},
		{
			f: func() slog.Handler {
				return &testutil.SlowHandler{Delay: 10 * time.Millisecond, Handler: testutil.NopHandler{}}
			},
			label: "10ms-delay",
		},
		{
			f:     func() slog.Handler { return &testutil.AllocatingHandler{N: 1024 * 4, FreeAt: 1024 * 1024 * 16} },
			label: "allocate 4KiB",
		},
		{
			f: func() slog.Handler {
				return &testutil.AllocatingHandler{N: 1024 * 1024 * 16, FreeAt: 1024 * 1024 * 16 * 10}
			},
			label: "allocate 16MiB",
		},
	} {
		b.Run(
			"handler="+handler.label, func(b *testing.B) {
				for _, parallelism := range []int{1, 8, 64, 512, 8192, 32768} {
					b.Run(
						"parallelism="+strconv.Itoa(parallelism), func(b *testing.B) {
							b.SetParallelism(parallelism)

							for _, tt := range tests {
								b.Run(
									tt.name, func(b *testing.B) {
										h := loglimit.MustNew(
											loglimit.Config{
												Burst:         loglimit.TokensByLevel{},
												Handler:       handler.f(),
												SampleRate:    tt.rate,
												Timing:        time.Tick(100 * time.Millisecond),
												TokensPerTick: tt.perTick,
											},
										)
										ctx := context.TODO()
										log := slog.New(h)
										b.ResetTimer()
										b.RunParallel(
											func(pb *testing.PB) {
												for pb.Next() {
													log.Log(
														ctx,
														ordering[a.Add(1)%int64(len(ordering))],
														"msg",
														"hello",
														"world",
													)
												}
											},
										)
									},
								)
							}
						},
					)
				}
			},
		)
	}
}
