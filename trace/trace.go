// Package trace provides utilities for tracing requests through and across services.
// It includes functionality for generating trace IDs and request IDs, saving them to context,
// and retrieving them from HTTP headers or context.
package trace

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/grene-labs/grene/enve"
)

// Trace contains the trace ID and request ID for a request.
// It is used to correlate logs and traces across different services.
type Trace struct {
	// TraceID is the trace ID that may span multiple requests, covering one logical
	// transaction. It is used to correlate logs and traces across different
	// services.
	// By convention, it is a UUID with optional prefix.
	// All TraceIDs should be unique across the system.
	// As a header, it is "X-Trace-ID".,
	TraceID string `json:"trace_id"`
	// RequestID is the request ID for just this network request.
	// It should NOT span multiple requests.
	// By convention, it is a UUID with optional prefix.
	// All RequestIDs should be unique across the system.
	// As a header, it is "X-Request-ID".
	RequestID string `json:"request_id"`
}

// ServerTrace is a middleware that adds a trace to the request context.
// This is used to propagate the trace ID and request ID to the next handler.
// ServerTrace should be used BEFORE ServerLogger.
func ServerMiddleware(next http.Handler) http.Handler {
	if next == nil {
		panic("next handler is nil")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// get the trace from the request header
		t, ok := FromHTTPHeader(req.Header)
		if !ok {
			t = New()
		}
		ctx := t.SaveCtx(req.Context())

		// save the trace to the request context
		req = req.WithContext(ctx)

		w.Header().Set("X-Request-ID", t.RequestID)
		w.Header().Set("X-Trace-ID", t.TraceID)

		// call the next handler
		next.ServeHTTP(w, req)
	})
}

// TraceIDHeader is the HTTP and Kafka header name for the TraceID.
// RequestIDHeader is the HTTP and Kafka header name for the RequestID.
const (
	TraceIDHeader   = "X-Trace-ID"
	RequestIDHeader = "X-Request-ID"
)

var (
	idPrefix  = enve.StringOr("TRACE_ID_PREFIX", enve.StringOr("DORA_ENV", ""))
	reqPrefix = enve.StringOr("REQUEST_ID_PREFIX", enve.StringOr("DORA_ENV", ""))
)

// SaveHTTPHeader saves the TraceID and RequestID to the http.Header.
func (t *Trace) SaveHTTPHeader(h http.Header) {
	h.Set(TraceIDHeader, t.TraceID)
	h.Set(RequestIDHeader, t.RequestID)
}

type ctxKey struct{}

// SaveCtx saves the Trace to the context.Context, returning a new context.Context.
func (t *Trace) SaveCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

func New() *Trace {
	return &Trace{
		TraceID:   newID(idPrefix),
		RequestID: newID(reqPrefix),
	}
}

// FromCtx retrieves the Trace from the context.Context, if it exists.
func FromCtx(ctx context.Context) (*Trace, bool) {
	t, ok := ctx.Value(ctxKey{}).(*Trace)
	if !ok {
		return nil, false
	}
	return t, true
}

// FromCtxOrNew retrieves the Trace from the context.Context, if it exists.
// If it does not exist, it creates a new Trace with new UUIDs for both TraceID and RequestID.
// The returned context will contain the returned Trace.
func FromCtxOrNew(ctx context.Context) (context.Context, *Trace) {
	if ctx == nil {
		ctx = context.Background()
	}
	t, _ := FromCtx(ctx)
	if t == nil {
		t = &Trace{
			TraceID:   newID(idPrefix),
			RequestID: newID(reqPrefix),
		}
	}
	return t.SaveCtx(ctx), t
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func newID(prefix string) string {
	if prefix == "" {
		return must(uuid.NewV7()).String()
	}
	if prefix[len(prefix)-1] == '-' {
		return prefix + must(uuid.NewV7()).String()
	}
	return prefix + "-" + must(uuid.NewV7()).String()
}

// FromHTTPHeader retrieves the Trace from the http.Header, if it exists.
// Partially filled headers are allowed and the missing values will be filled with new UUIDs.
// If both headers are empty, it returns nil and false.
func FromHTTPHeader(h http.Header) (*Trace, bool) {
	traceID, requestID := h.Get(TraceIDHeader), h.Get(RequestIDHeader)
	if traceID == "" && requestID == "" {
		return nil, false
	}
	if requestID == "" {
		requestID = newID(reqPrefix)
	}
	if traceID == "" {
		traceID = newID(idPrefix)
	}
	t := &Trace{
		TraceID:   traceID,
		RequestID: requestID,
	}

	return t, true
}
