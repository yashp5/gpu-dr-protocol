package testutil

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

type DemoHandler struct {
	W io.Writer
}

var _ slog.Handler = DemoHandler{}

// Enabled always returns true.
func (h DemoHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

// Handle writes the log message to the writer, ignoring log level, metadata, etc.
func (h DemoHandler) Handle(_ context.Context, r slog.Record) error {
	_, err := fmt.Fprintf(h.W, "%s %s\n", r.Level, r.Message)
	return err
}

// WithGroup does nothing.
func (h DemoHandler) WithGroup(g string) slog.Handler {
	return h
}

// WithAttrs does nothing.
func (h DemoHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}
