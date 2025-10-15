package logger

import (
	"context"
	"log/slog"
	"testing"

	"github.com/dusted-go/logging/prettylog"
)

// Test is a slog.Logger that writes to the underlying test.
func NewTest(t testing.TB, opts *slog.HandlerOptions) *slog.Logger {
	t.Helper()
	tw := &testWriter{T: t}
	if opts == nil {
		opts = &slog.HandlerOptions{Level: slog.LevelDebug}
	}
	h := prettylog.New(opts, prettylog.WithDestinationWriter(tw))
	return slog.New(h).With("test", t.Name())
}

// TestCtx a wrapper for logger.SaveCtx(t.Context(), logger.NewTest(t, nil)),
func TestCtx(t testing.TB) context.Context {
	return SaveCtx(t.Context(), NewTest(t, nil))
}

// NewGlobalTest is as NewTest, but replaces the global slog.Default logger with the one created by Test,
// restoring the old logger after the test ends.
// # Warning: Thread-Safety
//
//	Multiple tests running in parallel that replace the global logger will step on each other.
//	 Use the ordinary NewTest in those cases.
func NewGlobalTest(t testing.TB, opts *slog.HandlerOptions) *slog.Logger {
	t.Helper()
	oldlog := slog.Default()
	log := NewTest(t, opts)
	t.Cleanup(func() {
		slog.SetDefault(oldlog)
	})
	slog.SetDefault(log)
	return log
}

// testWriter is a simple io.Writer that logs to the testing.TB interface.
type testWriter struct {
	T testing.TB
}

func (tw *testWriter) Write(p []byte) (n int, err error) {
	tw.T.Helper()
	tw.T.Logf("%s: %s", tw.T.Name(), p)
	return len(p), nil
}
