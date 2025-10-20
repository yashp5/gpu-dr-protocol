package testutil

import (
	"context"
	crand "crypto/rand"
	"log/slog"
	"sync"
)

// AllocatingHandler is a slog.Handler that allocates N bytes on each log until FreeAt is reached.
// It then frees the buffer and starts over.
type AllocatingHandler struct {
	mux    sync.Mutex
	N      int
	FreeAt int
	b      []byte
}

func (a *AllocatingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (a *AllocatingHandler) WithGroup(g string) slog.Handler {
	return a
}

func (a *AllocatingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return a
}

func (a *AllocatingHandler) Handle(ctx context.Context, r slog.Record) error {
	b := make([]byte, a.N)
	crand.Read(b)
	a.mux.Lock()
	defer a.mux.Unlock()
	a.b = append(a.b, b...)
	if len(a.b) >= a.FreeAt {
		a.b = nil // free the buffer
	}
	return nil
}
