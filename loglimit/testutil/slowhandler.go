package testutil

import (
	"context"
	"log/slog"
	"time"
)

// SlowHandler is a slog.Handler that intentionally delays during Handle() to simulate logging latency.
type SlowHandler struct {
	Delay   time.Duration
	Handler slog.Handler
}

// Handle sleeps for Delay before calling the underlying handler.
func (s *SlowHandler) Handle(ctx context.Context, r slog.Record) error {
	time.Sleep(s.Delay)
	return s.Handler.Handle(ctx, r)
}

// Enabled returns the result of the underlying handler's Enabled method.
func (s *SlowHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return s.Handler.Enabled(ctx, level)
}

// WithGroup returns a new SlowHandler with the same delay and the underlying handler's WithGroup method.
func (s *SlowHandler) WithGroup(g string) slog.Handler {
	return &SlowHandler{
		Delay:   s.Delay,
		Handler: s.Handler.WithGroup(g),
	}
}

// WithAttrs returns a new SlowHandler with the same delay and the underlying handler's WithAttrs method.
func (s *SlowHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &SlowHandler{
		Delay:   s.Delay,
		Handler: s.Handler.WithAttrs(a),
	}
}
