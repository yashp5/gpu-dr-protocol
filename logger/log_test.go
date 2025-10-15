package logger_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/grene-labs/grene/logger"
)

func TestLog(t *testing.T) {
	// these tests are not parallel and can't be.

	var buf strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	slog.Info("want1") // we expect to see this in the buffer.

	t.Run("subtest", func(t *testing.T) {
		log := logger.NewGlobalTest(t, &slog.HandlerOptions{Level: slog.LevelDebug})
		log.Info("the buffer should not contain this")
	})
	slog.Info("want2") // we also expect to see this in the buffer, since the old log should be restored once the subtest ends.
	got := buf.String()
	if !strings.Contains(got, "want1") || !strings.Contains(got, "want2") {
		t.Errorf("expected to see %q and %q in the log, got:\n%s", "want1", "want2", got)
	}
	if strings.Contains(got, "the buffer should not contain this") {
		t.Errorf("expected not to see 'the buffer should not contain this' in the log, got:\n%s", got)
	}
}

func TestCtx(t *testing.T) {
	// these tests are not parallel and can't be.
	var globalbuf strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&globalbuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	t.Run("fallback to global default", func(t *testing.T) {
		log := logger.FromCtx(context.TODO())
		if log == nil {
			t.Fatal("expected non-nil logger from context")
		}
		log.Info("want global")
		if !strings.Contains(globalbuf.String(), "want global") {
			t.Errorf("expected to see 'want global' in the global log, got:\n%s", globalbuf.String())
		}
	})
	t.Run("save and retrieve logger from context", func(t *testing.T) {
		var localbuf strings.Builder
		ctx := logger.SaveCtx(logger.TestCtx(t), slog.New(slog.NewTextHandler(&localbuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		logger.FromCtx(ctx).Info("want local")
		if !strings.Contains(localbuf.String(), "want local") {
			t.Errorf("expected to see 'want local' in the local log, got:\n%s", localbuf.String())
		}
	})
}
