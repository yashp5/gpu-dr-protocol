package protocol

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/google/uuid"
	"github.com/grene-labs/grene/logger"
	"github.com/grene-labs/grene/trace"
	"github.com/klauspost/compress/zstd"
	"github.com/pingcap/log"
)

type (
	compressingResponseWriter struct {
		underlying http.ResponseWriter
		encoder    io.Writer
	}
)

// panicHandlerMiddleware is a middleware that recovers from panics in the handler chain using  the request's context's logger (see logger.FromCtx).
// It will log the panic and return a 500 Internal Server Error response in the following format:
//
//	{
//	  "error": "panic: <panic message>",
//	  "panic_id": "<panic ID>",
//	  "request_id": "<request ID>",
//	  "trace_id": "<trace ID>"
//	}
func panicHandlerMiddleware(h http.Handler) http.Handler {
	log.Debug("applying middleware: server panic handler")
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		log := logger.FromCtx(ctx)
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("panic: %v", r)
				panicID := must(uuid.NewV7()).String()
				tr, ok := trace.FromHTTPHeader(req.Header)
				if !ok {
					tr = trace.New()
				}

				log.ErrorContext(
					ctx, "panic in http handler",
					"err", err,
					"stack", string(debug.Stack()),
					"method", req.Method,
					"path", req.URL.Path,
					"remote_addr", req.RemoteAddr,
					"request_id", tr.RequestID,
					"trace_id", tr.TraceID,
					"panic_id", panicID,
				)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(
					struct {
						Error     string `json:"error"`
						PanicID   string `json:"panic_id"`
						RequestID string `json:"request_id"`
						TraceID   string `json:"trace_id"`
					}{
						Error:     err.Error(),
						PanicID:   panicID,
						RequestID: tr.RequestID,
						TraceID:   tr.TraceID,
					},
				)

			}
		}()
		h.ServeHTTP(w, req)
	})
}

/* TODO: write tests for these, then re-enable.
// ServerAcceptCompression is a middleware that accepts zstd and gzip compressed requests.
// It will decode the request body and remove the Content-Encoding header.
// It will return 415 Unsupported Media Type if the Content-Encoding is not supported.
// See ../internal/encoding/encoding.go for details on the encoders.
func ServerAcceptCompression(next http.Handler, lvl zstd.EncoderLevel) http.Handler {
	if next == nil {
		panic("next handler is nil")
	}
	if lvl == 0 {
		lvl = zstd.SpeedDefault
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch enc := strings.ToLower(req.Header.Get("Content-Encoding")); enc {
		case "gzip", "x-gzip", "deflate": // you'll see all 3 in the wild for ordinary gzip
			// usually this won't actually trigger, since go's HTTP client & server automatically handle gzip
			// but it's nice to have it here for completeness
			req.Body = must(gzip.NewReader(req.Body))
			req.Header.Del("Content-Encoding") // remove the Content-Encoding header so the next handler doesn't try to decode it again
		case "zstd", "x-zstd":
			req.Body = &readcloser{r: must(zstd.NewReader(req.Body)), underlying: req.Body}
			req.Header.Del("Content-Encoding") // remove the Content-Encoding header so the next handler doesn't try to decode it again
		case "":
			// no encoding, do nothing
		default:
			// unsupported encoding, return 415 Unsupported Media Type
			http.Error(w, "Unsupported Content-Encoding", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, req)
	})
}
*/

// serverCompressResponsesMiddleware is a middleware that responds to requests with 'Accept-Encoding: gzip' or 'Accept-Encoding: zstd' by compressing the response body w/ the relevant encoder.
// It will set the Content-Encoding header to 'gzip' or 'zstd', as appropriate.
// 0 is the default compression level and OK for most use cases.
// If no encoding, or an unknown encoding is specified, it will not compress the response.
// See ../internal/encoding/encoding.go for details on the encoders.
func serverCompressResponsesMiddleware(next http.Handler, gzipLevel int, zstdLevel zstd.EncoderLevel) http.HandlerFunc {
	if next == nil {
		panic("next handler is nil")
	}
	if gzipLevel == 0 {
		gzipLevel = gzip.DefaultCompression
	}
	if zstdLevel == 0 {
		zstdLevel = zstd.SpeedDefault
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// check if the request accepts gzip or zstd. we prioritize zstd over gzip: it's more efficient.
		var foundGzip, foundZstd bool
		if r.Header.Get("upgrade") != "" {
			// TODO: if we switch to using a different server entirely for websockets, we can remove this check,
			// since we simply won't apply this middleware there.
			next.ServeHTTP(w, r) // don't compress if the request is an upgrade request (e.g. WebSocket)
			return
		}
		for _, enc := range r.Header.Values("Accept-Encoding") {
			if strings.Contains(enc, "zstd") {
				foundZstd = true
				break
			}
			if strings.Contains(enc, "gzip") || strings.Contains(enc, "deflate") {
				foundGzip = true
			}
		}
		switch {
		case foundZstd: // zstd is preferred over gzip
			r.Header.Del("Accept-Encoding") // we don't want a later middleware to do this again, so remove it.
			w.Header().Set("Content-Encoding", "zstd")
			encoder := must(zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevel(zstdLevel))))
			defer encoder.Close()
			w = &compressingResponseWriter{
				encoder:    encoder,
				underlying: w,
			}

		case foundGzip:
			w.Header().Set("Content-Encoding", "gzip")
			r.Header.Del("Accept-Encoding") // we don't want a later middleware to do this again, so remove it.
			encoder := must(gzip.NewWriterLevel(w, gzipLevel))
			defer encoder.Close()
			w = &compressingResponseWriter{
				encoder:    encoder,
				underlying: w,
			}

		default:
			// no-op
		}
		// call the next handler
		next.ServeHTTP(w, r)
	}
}

// Unwrap returns the underlying ResponseWriter, allowing access to the original writer.
// This is necessary for websocket support so we can get to the underlying http.Hijacker.
// See DORA-3420 for an example of where this crops up.
func (w *compressingResponseWriter) Unwrap() http.ResponseWriter {
	return w.underlying
}

// Write writes the compressed response via the encoder to the underlying ResponseWriter.
func (w *compressingResponseWriter) Write(b []byte) (int, error) { return w.encoder.Write(b) }

// Header returns the header map of the underlying ResponseWriter.
func (w *compressingResponseWriter) Header() http.Header { return w.underlying.Header() }

// WriteHeader writes the header to the underlying ResponseWriter, without compression (since the header is not compressed and is BEFORE the body).
func (w *compressingResponseWriter) WriteHeader(code int) { w.underlying.WriteHeader(code) }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
