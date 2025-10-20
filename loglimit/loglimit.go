// Package loglimit provides a set of handlers that limit the number of logs emitted by a *slog.Logger via token buckets and statistical sampling.
//
// # Why this package?
//
//   - Simple, straightforward API that works with the standard library's log/slog package without fuss.
//   - Good performance: all handlers are lock & allocation free after initialization. See benchmarks.md for details.
//   - Full test coverage, including examples for all features.
//   - No dependencies.
package loglimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync/atomic"
	"time"
)

type (
	// Handler is a slog.Handler that limits the number of logs emitted by the underlying handler.
	// Use this to protect your servers (or your finances) from being overwhelmed by unexpected log traffic.
	//
	// # Building a Handler
	//
	// - New() is the most flexible way to build a Handler. See [New] and [Config] for details.
	// - [NewTokenBucket]() is a convenience function that limits logs emitted by the underlying handler via token buckets, without statistical sampling.
	// - NewSampler() is a convenience function that limits logs emitted by the underlying handler via statistical sampling, without token buckets.
	Handler struct {
		handler slog.Handler // underlying handler
		*shared              // share a pointer to everything that doesn't change between instances so .WithAttrs() and .WithGroup() are cheap.
	}

	// TokensByLevel is a mapping of log levels to the number of logs that should be let through per tick.
	TokensByLevel struct{ Debug, Info, Warn, Error int64 }
	// Mapping of log levels to the rate at which logs should be sampled.
	// Must in the range [0, 1]. 0 means no logs are let through, 1 means all logs are let through.
	SampleRateByLevel struct{ Debug, Info, Warn, Error float64 }
	shared            struct {
		_              noCopy
		timing         <-chan time.Time
		stats          stats
		perTick, burst TokensByLevel
		rate           SampleRateByLevel
		rng            func() float64
		tokens         struct {
			Debug, Info, Warn, Error atomic.Int64
		}
	}
	noCopy [0]func()
)

func (_ *noCopy) Lock() {} // signal to go vet that this type should never be copied by value

// NewTokenBucket limits logs emitted by the underlying handler via token buckets, without statistical sampling.
// Every tick on the timing channel, perTick.level logs are let through.
//
// See New() for a more customizable API.
func NewTokenBucket(timing <-chan time.Time, handler slog.Handler, perTick TokensByLevel) *Handler {
	return MustNew(Config{Timing: timing, Handler: handler, TokensPerTick: perTick})
}

// NewSampler creates a new LimitHandler, which limits the number of logs emitted by the underlying handler via statistical sampling, without token buckets.
// See New() for a more customizable API.
func NewSampler(timing <-chan time.Time, handler slog.Handler, sampleRate SampleRateByLevel) *Handler {
	return MustNew(Config{Timing: timing, Handler: handler, SampleRate: sampleRate})
}

// Configuration for building a [Handler] via [New].
type Config struct {
	// Timing is the channel that controls the timing of the token bucket.
	Timing <-chan time.Time
	// Handler is the underlying handler that will be limited.
	Handler slog.Handler

	// Optional. SampleRate is the rate at which logs should be sampled after the perTick limit is reached.
	SampleRate SampleRateByLevel
	// Optional. TokensPerTick is the number of tokens (by level) for the bucket that should be added per tick.
	TokensPerTick TokensByLevel

	// Optional. Burst is additional capacity for the bucket, beyond the perTick limit. Unused $LEVEL tokens can be stored up to this value.
	Burst TokensByLevel
}

// Validate the configuration.
func (cfg Config) Validate() error {
	switch {
	case cfg.Timing == nil:
		return errors.New("missing mandatory field 'Timing'")
	case cfg.Handler == nil:
		return errors.New("missing mandatory field 'Handler'")
	case cfg.TokensPerTick.Debug < 0, cfg.TokensPerTick.Info < 0, cfg.TokensPerTick.Warn < 0, cfg.TokensPerTick.Error < 0:
		return fmt.Errorf("expected all perTick values to be non-negative, got %+v", cfg.TokensPerTick)
	case /**/ cfg.SampleRate.Debug < 0, cfg.SampleRate.Info < 0, cfg.SampleRate.Warn < 0, cfg.SampleRate.Error < 0,
		/* */ cfg.SampleRate.Debug > 1, cfg.SampleRate.Info > 1, cfg.SampleRate.Warn > 1, cfg.SampleRate.Error > 1:
		return fmt.Errorf("expected all sample rates to be in range [0, 1], got %+v", cfg.SampleRate)
	case cfg.Burst.Debug < 0, cfg.Burst.Info < 0, cfg.Burst.Warn < 0, cfg.Burst.Error < 0:
		return fmt.Errorf("expected all burst values to be non-negative, got %+v", cfg.Burst)
	default:
		return nil
	}
}

// MustNew is as New, but panics if the configuration is invalid rather than returning an error.
func MustNew(cfg Config) *Handler {
	h, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return h
}

// New creates a new LimitHandler, which limits the number of logs emitted by the underlying handler using a combination of token buckets and statistical sampling.
//   - for each level, perTick.level logs are let through every tick on the timing channel.
//   - after the limit is reached, logs are sampled at the rate sampleRate.level.
//   - the sampling rate is per level, not global.
func New(cfg Config) (*Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %T: %w", cfg, err)
	}

	h := &Handler{
		handler: cfg.Handler,
		shared: &shared{
			burst:   cfg.Burst,
			perTick: cfg.TokensPerTick,
			rate:    cfg.SampleRate,
			rng:     rand.Float64,
			timing:  cfg.Timing,
		},
	}

	{
		h.tokens.Debug.Store(h.perTick.Debug)
		h.tokens.Info.Store(h.perTick.Info)
		h.tokens.Warn.Store(h.perTick.Warn)
		h.tokens.Error.Store(h.perTick.Error)
	}
	// no burst mode
	if h.burst.Debug == 0 && h.burst.Info == 0 && h.burst.Warn == 0 && h.burst.Error == 0 {
		go func() {
			// Reset counters every interval tick.
			for range h.timing {
				h.tokens.Debug.Store(h.perTick.Debug)
				h.tokens.Info.Store(h.perTick.Info)
				h.tokens.Warn.Store(h.perTick.Warn)
				h.tokens.Error.Store(h.perTick.Error)
			}
		}()
		return h, nil
	}

	// burst mode
	go func() {
		// Reset counters every interval tick.
		for range h.timing {
			dbg := h.tokens.Debug.Swap(h.perTick.Debug)
			info := h.tokens.Info.Swap(h.perTick.Info)
			warn := h.tokens.Warn.Swap(h.perTick.Warn)
			err := h.tokens.Error.Swap(h.perTick.Error)

			if dbg > 0 {
				h.tokens.Debug.Add(dbg)
			}

			if info > 0 {
				h.tokens.Info.Add(info)
			}

			if warn > 0 {
				h.tokens.Warn.Add(warn)
			}

			if err > 0 {
				h.tokens.Error.Add(err)
			}
		}
	}()
	return h, nil
}

// Handler returns the underlying handler used to create this LimitHandler, "unwrapping" it.
// In general, you should avoid using this method, but I'd rather give you the option than make the API hellish
// if you think of something else you need.
func (h *Handler) Handler() slog.Handler { return h.handler }

// WithAttrs returns a new Handler whose attributes consist of both the receiver's attributes and the arguments.
// It does so by delegating to the underlying handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{handler: h.handler.WithAttrs(attrs), shared: h.shared}
}

// WithGroup returns a new Handler with the given group appended to the receiver's existing groups by delegating to the underlying handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{handler: h.handler.WithGroup(name), shared: h.shared}
}

// Handle the record, delegating to the underlying handler. This is only called if Enabled returns true.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error { return h.handler.Handle(ctx, r) }

// Enabled implements the slog.Handler interface.
// Each level has its own counter.
// Up to level.counter logs are let through every tick on the timing channel.
// If the number of logs exceeds the counter, sample sampleRate.level of the logs.
// Log levels outside of the standard slog levels (Debug, Info, Warn, Error) are always let through.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	res := h.enabled(ctx, level)
	h.stats.update(level, res)
	return res == allowed
}

// enabled returns whether a log of the given level should be let through,
func (h *Handler) enabled(ctx context.Context, level slog.Level) logEnabledResult {
	if !h.handler.Enabled(ctx, level) { // if the underlying handler is disabled, don't use up the quota
		return droppedByParent
	}

	if h.takeToken(level) {
		return allowed
	}
	var target float64
	switch level {
	default:
		return allowed
	case slog.LevelDebug:
		target = h.rate.Debug
	case slog.LevelInfo:
		target = h.rate.Info
	case slog.LevelWarn:
		target = h.rate.Warn
	case slog.LevelError:
		target = h.rate.Error
	}
	switch {
	case target <= 0: // no need for a function call
		return dropped
	case target >= 1: // no need for a function call
		return allowed
	case h.rng() <= target:
		return allowed
	default:
		return dropped
	}
}

// take a token from the bucket for the given level, if you can.
func (s *shared) takeToken(level slog.Level) bool {
	switch level {
	default:
		return true
	case slog.LevelDebug:
		return s.tokens.Debug.Add(-1) >= 0
	case slog.LevelInfo:
		return s.tokens.Info.Add(-1) >= 0
	case slog.LevelWarn:
		return s.tokens.Warn.Add(-1) >= 0
	case slog.LevelError:
		return s.tokens.Error.Add(-1) >= 0
	}
}
