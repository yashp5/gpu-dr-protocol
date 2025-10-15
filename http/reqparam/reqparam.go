// Package reqparam provides utilities for parsing HTTP request parameters.
// Parameters come in four kinds:
//   - Path parameters (e.g., `/users/{id}`)
//   - Query parameters (e.g., `/users?id=123`)
//   - Header parameters (e.g., `X-User-ID: 123`)
//   - JSON Body parameters (e.g., {"id": 123})
package reqparam

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"runtime/debug"

	"github.com/grene-labs/grene/http/writeresp"
	"github.com/grene-labs/grene/logger"
)

// CatchMustMiddleware catches panics from MustPath, MustQuery, MustHeader, and MustBody, giving a 400 Bad Request response.
func CatchMustMiddleware(next http.Handler) http.Handler {
	if next == nil {
		panic("next handler is nil")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if r := recover(); r != nil {
				var as *ParamError
				log := logger.FromCtx(req.Context())
				if err, ok := r.(error); ok && errors.As(err, &as) {
					switch as.Kind {
					case KindBody:
						log.Warn(fmt.Sprintf("%s %s: 400 bad request: bad body: %s", req.Method, req.URL.Path, as.Err.Error()),
							slog.Group("param", "type", as.Type.String(), "body", as.Value))
					default:
						log.Info(fmt.Sprintf("%s %s: 400 bad request: bad %s parameter %s", req.Method, req.URL.Path, as.Kind, as.Key),
							slog.Group("param",
								"key", as.Key,
								"value", as.Value,
								"kind", string(as.Kind),
								"type", as.Type.String(),
								"error", as.Err.Error(),
							))
					}
					writeresp.JSONErr(req.Context(), w, as, http.StatusBadRequest)
				} else {
					log.Error("panic in http handler: stack trace", "stack", string(debug.Stack()))
					panic(r) // re-panic if it's not a ParamError
				}
			}
		}()
		next.ServeHTTP(w, req)
	})
}

// Kind of parameter. One of:
// - KindPath: "path": path parameters (e.g., `/users/{id}`)
// - KindQuery: "query": query parameters (e.g., `/users?id=123
// - KindHeader: "header": header parameters (e.g., `X-User-ID: 123`)
// - KindBody: "body": JSON body parameters (e.g., {"id": 123})
type ParamKind string

var (
	ErrNoParam      = errors.New("no parameter")
	ErrInvalidParam = errors.New("invalid parameter")
)

// ParseScanner is a helper function to parse a value into a sql.Scanner type.
func ParseScanner[T any, PT interface {
	// note on this generic type signature:
	// we want a value of type T, but since Scan() works on a _POINTER_, we can't just have a single type parameter T.
	// read this as "some type PT which is a p
	*T
	sql.Scanner
}](s string) (t T, err error) {
	err = PT(&t /* cast the pointer to the specific pointer type PT */).Scan(s) // use the sql.Scanner interface to parse the value
	return t, err
}

// Kinds of parameters for HTTP requests.
// See ParamKind for details.
const (
	KindPath   ParamKind = "path"
	KindQuery  ParamKind = "query"
	KindHeader ParamKind = "header"
	KindBody   ParamKind = "body"
)

// ParamError is an error type for HTTP request parameters.
// All non-nil errors returned by this package are of type *ParamError and
// will have nonzero Kind, Key, Err, and Type fields.
// The Value may be nil if the parameter was not present, such as empty JSON bodies.
// You can use .Unwrap(), errors.Is(), and errors.As() to check for specific underlying errors:
// see the errors package documentation for more details.
type ParamError struct {
	Kind  ParamKind
	Key   string
	Value any
	Err   error
	Type  reflect.Type
}

// Error message like "parse HTTP path parameter "id" into a uuid.UUID: invalid UUID length: 12"
func (e *ParamError) Error() string {
	switch e.Kind {
	case KindBody:
		var buf bytes.Buffer
		b, _ := e.Value.([]byte)
		err := json.Indent(&buf, b, "", "  ")
		if err == nil {
			b = buf.Bytes()
		}
		return fmt.Sprintf("parse JSON body into a %s: %s\n\nbody:\t%s", e.Type, e.Err, b)
	default:
		return fmt.Sprintf("parse HTTP %s parameter %q into a %s: %s\nvalue:%v", e.Kind, e.Key, e.Type, e.Err, e.Value)
	}
}

// Unwrap returns the underlying error for use by errors.Is and errors.As.
func (e *ParamError) Unwrap() error { return e.Err }

// StatusCode returns 400 Bad Request for PathError. This will help surface the proper error to the user for MustPath.
func (e *ParamError) StatusCode() int { return http.StatusBadRequest }

func must[T any](q T, err error) T {
	if err != nil {
		panic(err)
	}
	return q
}
