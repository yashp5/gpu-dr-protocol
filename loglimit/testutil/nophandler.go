package testutil

import (
	"context"
	"log/slog"
)

// NopHandler implements slog.Handler by doing nothing.
type NopHandler struct{}

var _ slog.Handler = NopHandler{}

// Enabled always returns true.
func (NopHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle does nothing and always returns nil.
func (NopHandler) Handle(context.Context, slog.Record) error { return nil }

// WithGroup does nothing and returns the receiver.
func (NopHandler) WithGroup(string) slog.Handler { return NopHandler{} }

// WithAttrs does nothing and returns the receiver.
func (NopHandler) WithAttrs([]slog.Attr) slog.Handler { return NopHandler{} }
