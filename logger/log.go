package logger

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/grene-labs/grene/trace"
)

type (
	// Log levels for ServerMiddleware.
	MiddlewareConfig struct {
		Req                                slog.Level // request
		Resp2xx, Resp3xx, Resp4xx, Resp5xx slog.Level // response by status code
		RespUnknownStatus                  slog.Level // response with an unknown status code
	}

	// recordingResponseWriter is a wrapper around http.ResponseWriter that records the status code and # of bytes written.
	// see ServerMiddleware for usage.
	recordingResponseWriter struct {
		inner      http.ResponseWriter
		ctx        context.Context // the context with the logger
		n          int             // count of bytes written
		statusCode int
	}

	_ctxkey struct{} // used by FromCtx and SaveCtx to store the logger in the context.Context.
)

var (
	ctxkey           _ctxkey  // the key used to store the logger in the context.Context
	missingLoggerMap sync.Map // map of file:line to sync.Once. only used by FromCtx.
)

// Return a child context with the logger set, for later retrieval via FromCtx.
func SaveCtx(ctx context.Context, log *slog.Logger) context.Context {
	if ctx == nil {
		panic("context is nil")
	}
	if log == nil {
		panic("logger is nil")
	}
	return context.WithValue(ctx, ctxkey, log)
}

// FromCtx retrieves a logger previously saved by SaveCtx, or the default logger if none is found.
// It will log exactly once per file:line where the context is nil or does not contain a logger.
func FromCtx(ctx context.Context) (log *slog.Logger) {
	// for missingLoggerMap. a combination of file and line number to ensure we only log once per file:line
	type fileline struct {
		file string
		line int
	}

	if ctx == nil {
		panic("logger.FromCtx: nil context")
	}
	log, _ = ctx.Value(ctxkey).(*slog.Logger)
	if log == nil {
		log = slog.Default()
		_, file, line, ok := runtime.Caller(1) // 2 to skip this function and the caller
		if !ok {
			file = "unknown"
		}
		v, _ := missingLoggerMap.LoadOrStore(fileline{file: file, line: line}, new(sync.Once))
		v.(*sync.Once).Do(func() {
			log.Warn(fmt.Sprintf("%s:%d : no logger found in context, using default logger", file, line), "fileline", fmt.Sprintf("%s:%d", file, line))
		})
	}
	return log
}

// ServerMiddleware is middleware that adds a logger to the request context and logs the request and response at the specified levels.
// Use FromCtx(r.Context()) to retrieve the logger in later handlers.
func ServerMiddleware(next http.Handler, log *slog.Logger, levels MiddlewareConfig) http.HandlerFunc {
	if log == nil {
		panic("logger is nil")
	}
	if next == nil {
		panic("next handler is nil")
	}
	log.Debug("applying middleware: server logger")
	// crawl the stack to find the first non-middleware caller

	return http.HandlerFunc(
		func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			log := log // shadow the scope so requests don't interfere with each other or the passed-in logger

			// add extra context to the logger
			// ATTENTION: we could log additional request headers here, but this can leak Personally Identifiable Information (PII) and other sensitive data.
			// If we want to do this, we should have a whitelist of appropriate headers, OR just log the header KEYS and sizes, not the values.
			// we could also log the request body, but this is noisy and can leak PII.
			log = log.With(slog.Group("request",
				//"accept_encoding", req.Header.Get("Accept-Encoding"),
				//"accept", req.Header.Get("Accept"),
				//"content_length", req.Header.Get("Content-Length"),
				//"content_type", req.Header.Get("Content-Type"),
				"method", req.Method,
				"path", req.URL.Path,
				//"remote_addr", req.RemoteAddr,
				//"start", start,
				"user_agent", req.UserAgent(),
			))

			t, ok := trace.FromCtx(req.Context())
			if !ok {
				t, ok = trace.FromHTTPHeader(req.Header)
			}
			if ok {
				log = log.With("trace_id", t.TraceID, "request_id", t.RequestID)
			}

			// substitute out the request and response writers with the logging ones
			ctx := SaveCtx(req.Context(), log)
			req = req.WithContext(ctx)
			log.Log(ctx, levels.Req, fmt.Sprintf("%s %s: begin", req.Method, req.URL.Path))
			recorder := &recordingResponseWriter{ctx: ctx, inner: w}

			// actually call the next handler
			next.ServeHTTP(recorder, req)

			{ // we've now recorded the response. log it.

				elapsed := time.Since(start)
				var lvl slog.Level
				switch sc := recorder.statusCode; {
				case sc >= 200 && sc < 300:
					lvl = levels.Resp2xx
				case sc >= 300 && sc < 400:
					lvl = levels.Resp3xx
				case sc >= 400 && sc < 500:
					lvl = levels.Resp4xx
				case sc >= 500 && sc < 600:
					lvl = levels.Resp5xx
				default: // unknown status code
					lvl = levels.RespUnknownStatus
				}
				// ATTENTION: we could log response headers here, but this can leak Personally Identifiable Information (PII) and other sensitive data.
				// If we want to do this, we should have a whitelist of appropriate headers, OR just log the header KEYS and sizes, not the values.
				// we could also log the response body, but this is noisy and can leak PII.
				msg := fmt.Sprintf(
					"%s %s: %d %s",
					req.Method,
					req.URL.Path,
					recorder.statusCode,
					http.StatusText(recorder.statusCode),
				)
				log.Log(
					ctx, lvl, msg,
					"status_code", recorder.statusCode,
					"elapsed", elapsed.String(),
				)
			}
		},
	)
}

// Write the response header, recording the status code & then forwarding to the original ResponseWriter.
// Only the first call to WriteHeader will be recorded; subsequent calls will be ignored.
func (w *recordingResponseWriter) WriteHeader(code int) {
	if w.statusCode == 0 {
		w.statusCode = code // ignore subsequent calls to WriteHeader
		w.inner.WriteHeader(code)
		return
	}
	slog.WarnContext(w.ctx, "http: conflict: writeHeader called multiple times", "prev_status_code", w.statusCode, "new_status_code", code)
}

// Unwrap returns the original ResponseWriter, allowing access to the underlying writer if needed.
// This is necessary for websocket support so we can get to the underlying http.Hijacker.
// See DORA-3420 for an example of where this crops up.
func (w *recordingResponseWriter) Unwrap() http.ResponseWriter { return w.inner }

// Write the response body, recording the number of bytes written & then forwarding to the original ResponseWriter.
// If the status code has not been set, it will be set to http.StatusOK.
func (w *recordingResponseWriter) Write(b []byte) (int, error) {
	n, err := w.inner.Write(b)
	w.n += n
	return n, err
}

// Header returns the header map of the original ResponseWriter.
func (w *recordingResponseWriter) Header() http.Header {
	return w.inner.Header()
}

// ReplaceAttr formats and replaces the Attrs in a slog.Logger.
// It:
//   - replaces Source with <function> @ <file>:<line>
//   - formats time.Time as RFC3339 UTC (e.g., 2023-10-01T12:34:56Z)
//   - formats time.Duration as a string (2m30s)
//   - formats errors as strings and []error as single strings
func ReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.SourceKey {
		src, ok := a.Value.Any().(*slog.Source)
		if !ok {
			return a
		}
		// trim the file to the last 3 components, plus a .. if there's more than 3.
		fn := trimComponents(src.Function, 4)
		fn = strings.TrimSuffix(fn, "[...]")
		a.Value = slog.StringValue(fmt.Sprintf("%s @ %s:%d", fn, trimComponents(src.File, 3), src.Line))
		return a
	}
	switch k := a.Value.Kind(); k {
	default:
		return a
	case slog.KindTime: // make sure all times are always UITC everywhere.
		t := a.Value.Time().UTC()
		a.Value = slog.StringValue(t.Format(time.RFC3339))
		return a
	case slog.KindDuration: // turn duration into a string.
		d := a.Value.Duration()
		a.Value = slog.StringValue(d.String())
		return a
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case []error: // turn errors into strings.
			s := make([]string, 0, len(v))
			for _, err := range v {
				if err != nil {
					s = append(s, err.Error())
				}
			}

			a.Value = slog.AnyValue(s)
			return a
		case error: // turn a single error into a string.
			if v == nil {
				return slog.Attr{} // don't log nil errors.
			}
			a.Value = slog.StringValue(v.Error())
			return a
		default:
			return a
		}
	}
}

func trimComponents(s string, n int) string {
	components := strings.Split(s, string(os.PathSeparator))
	if len(components) > n {
		return strings.Join(components[len(components)-n:], string(os.PathSeparator))
	}
	return s
}
