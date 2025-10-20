package protocol

import (
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/cors"
	"github.com/grene-labs/grene/auth"
	"github.com/grene-labs/grene/cache"
	"github.com/grene-labs/grene/dbutil"
	"github.com/grene-labs/grene/enve"
	"github.com/grene-labs/grene/generated/sqlc"
	"github.com/grene-labs/grene/http/reqparam"
	"github.com/grene-labs/grene/http/writeresp"
	"github.com/grene-labs/grene/logger"
	"github.com/grene-labs/grene/openapi"
	"github.com/grene-labs/grene/trace"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"
)

// build the router
func buildHTTPRouter(
	log *slog.Logger,
	db interface {
		dbutil.DBTXandBegin
		Stat() *pgxpool.Stat
		Count() *dbutil.Count
	},
	c *cache.JSONResponse,
) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	type route struct {
		Method       string        `json:"method"`
		Path         string        `json:"path"`
		Handler      http.Handler  `json:"-"`
		CacheTTL     time.Duration `json:"cache_ttl,omitzero"` // optional cache TTL for this route. if not provided, the route is not cached.
		Description  string        `json:"description"`
		IsWebsocket  bool          `json:"is_websocket"`  // is this a websocket route? TODO: remove this when we split out websockets.
		AllowedRoles auth.Role     `json:"allowed_roles"` // optional allowed roles for this route. if not provided, this is available to the public.
	}
	q := sqlc.New(db)
	// build router & apply middleware
	mux := http.NewServeMux()

	routes := []route{
		{
			Method:      "GET",
			Path:        "/v1/health",
			Handler:     healthcheckHandler(db),
			CacheTTL:    0, // healthcheck handles caching internally.
			Description: "Health check endpoint",
		},
		{
			Method:      "GET",
			Path:        "/v1/openapi.json",
			Handler:     http.HandlerFunc(openapi.Handler),
			Description: "OpenAPI JSON specification",
		},
		// Debug
		{
			Method: "GET",
			Path:   "/v1/debug/pid",
			Handler: http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					writeresp.JSONData(r.Context(), w, os.Getpid(), http.StatusOK)
				},
			),
			Description: "write the process ID to the response",
		},
		{
			Method: "GET",
			Path:   "/v1/debug/poolstats",
			Handler: http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					stat := db.Stat()
					if stat == nil {
						writeresp.JSONErr(
							r.Context(),
							w,
							errors.New("no stats available: not a pgxpool?"),
							http.StatusServiceUnavailable,
						)
						return
					}

					writeresp.JSONData(r.Context(), w, dbutil.PoolStatToResponse(stat), http.StatusOK)
				},
			),
		},
		{
			Method:       "GET",
			Path:         "/v1/debug/pprof/block",
			Handler:      pprof.Handler("block"),
			AllowedRoles: auth.RoleAdmin,
		},
		{
			Method:       "GET",
			Path:         "/v1/debug/pprof/cmdline",
			Handler:      pprof.Handler("cmdline"),
			AllowedRoles: auth.RoleAdmin,
		},
		{
			Method:       "GET",
			Path:         "/v1/debug/pprof/goroutine",
			Handler:      pprof.Handler("goroutine"),
			AllowedRoles: auth.RoleAdmin,
		},
		{
			Method:       "GET",
			Path:         "/v1/debug/pprof/heap",
			Handler:      pprof.Handler("heap"),
			AllowedRoles: auth.RoleAdmin,
		},
		{
			Method:       "GET",
			Path:         "/v1/debug/pprof/profile",
			Handler:      http.HandlerFunc(pprof.Profile),
			AllowedRoles: auth.RoleAdmin,
		},
		{
			Method:       "GET",
			Path:         "/v1/debug/pprof/threadcreate",
			Handler:      pprof.Handler("threadcreate"),
			AllowedRoles: auth.RoleAdmin,
		},

		// DB: count by fileline, func, sql, caller
		{
			Method:      "GET",
			Path:        "/v1/debug/db/count/caller",
			Handler:     http.HandlerFunc(db.Count().ByCallerHandler),
			Description: "Get a count of queries by caller (file:line)",
		},
		{
			Method:      "GET",
			Path:        "/v1/debug/db/count/sql",
			Handler:     http.HandlerFunc(db.Count().BySQLHandler),
			Description: "Get a count of queries by SQL statement",
		},
		{
			Method:      "GET",
			Path:        "/v1/debug/db/count/fileline",
			Handler:     http.HandlerFunc(db.Count().ByFileLineHandler),
			Description: "Get a count of queries by file:line",
		},
		{
			Method:      "GET",
			Path:        "/v1/debug/db/count/func",
			Handler:     http.HandlerFunc(db.Count().ByFuncHandler),
			Description: "Get a count of queries by function",
		},
		// Users
		// {
		// 	Method:       http.MethodPost,
		// 	Path:         "/v1/user",
		// 	Handler:      user.Create(db),
		// 	Description:  "Create a new user",
		// 	AllowedRoles: auth.RoleAdmin,
		// },
		// {
		// 	Method:       http.MethodGet,
		// 	Path:         "/v1/user/{user_id}",
		// 	Handler:      user.GetByID(q.GetUserByID),
		// 	CacheTTL:     5 * time.Second,
		// 	Description:  "Get user info by user id",
		// 	AllowedRoles: auth.RoleAdmin,
		// },
	}

	// build an extra route, /, that returns the list of routes.
	// this is useful for debugging and testing.
	var routeraw json.RawMessage
	routeraw, _ = json.MarshalIndent(routes, "", "  ")
	routes = append(
		routes, route{
			Method: "GET",
			Path:   "/v1/{$}",
			Handler: http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					writeresp.JSONData(r.Context(), w, routeraw, http.StatusOK)
				},
			),
		},
	)

	// let's help our users out.
	// if they try and hit an API that doesn't exist (e.g, /v1/foo), tell them that they should try the bare path (/v1/) to get a list of routes.
	mux.HandleFunc(
		"/v1/", func(w http.ResponseWriter, r *http.Request) {
			writeresp.JSONErr(
				r.Context(),
				w,
				fmt.Errorf(
					"unknown route %s %s. try the bare path (%s) to get a list of routes",
					r.Method,
					r.URL.Path,
					strings.ReplaceAll(r.URL.String(), r.URL.Path, "/"),
				),
				http.StatusNotFound,
			)
		},
	)
	// if they hit anything which isn't an API at all, redirect them to /v1/ with a temporary redirect.Handler
	mux.HandleFunc(
		"/", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/v1/") {
				http.Redirect(w, r, "/v1"+r.URL.Path, http.StatusTemporaryRedirect)
				return
			}
			http.NotFound(w, r)
		},
	)

	slices.SortFunc(
		routes, func(a, b route) int {
			if c := strings.Compare(a.Path, b.Path); c != 0 {
				return c
			}
			return strings.Compare(a.Method, b.Method)
		},
	)

	start := time.Now()
	// build the router, associating middleware with the routes and logging which ones we're turning on.
	for _, route := range routes {
		h := route.Handler
		if route.CacheTTL > 0 { // apply the cache if and only if the route has a cache TTL.,
			if route.Method != http.MethodGet && route.Method != http.MethodHead {
				panic(
					fmt.Sprintf(
						"route %q %q has a cache TTL but is not a GET or HEAD request: programmer error?",
						route.Method,
						route.Path,
					),
				)
			}
			h = cache.Middleware(c, route.CacheTTL, h)
		}
		// Always enforce role checks; in BELIEVE_CLIENT mode, inject user (if any) before checking roles.
		h = auth.CheckUserRoleMiddleware(h, route.AllowedRoles)
		if enve.StringOr("PROTOCOL_AUTH", "true") == "BELIEVE_CLIENT" {
			h = auth.BelieveClientAboutUserMiddleware(h, q.GetUserByID)
		}

		mux.Handle(fmt.Sprintf("%s %s", route.Method, route.Path), h)
		if before, ok := strings.CutSuffix(route.Path, "v1"); ok {
			mux.HandleFunc(
				fmt.Sprintf("%s %s", route.Method, before),
				http.RedirectHandler(route.Path, http.StatusTemporaryRedirect).ServeHTTP,
			)
		}
	}
	var attrs []any
	for _, route := range routes {
		groupattrs := []any{
			"description", route.Description,
			"roles", route.AllowedRoles.String(),
			"websocket", route.IsWebsocket,
		}
		if route.CacheTTL > 0 {
			groupattrs = append(groupattrs, "cache_ttl", route.CacheTTL.String())
		}
		attrs = append(
			attrs,
			slog.Group(fmt.Sprintf("%s %s", route.Method, route.Path), groupattrs...),
		)
	}
	attrs = append(attrs, slog.String("duration", time.Since(start).String()))
	log.Debug("built routes", attrs...)

	logOr := func(s string) (slog.Level, error) {
		switch strings.ToLower(s) {
		case "debug":
			return slog.LevelDebug, nil
		case "info":
			return slog.LevelInfo, nil
		case "warn":
			return slog.LevelWarn, nil
		case "error":
			return slog.LevelError, nil
		default:
			return slog.LevelDebug, fmt.Errorf("invalid log level %q", s)
		}
	}

	var h http.Handler = mux
	{ // apply middleware, first-in, first-out.

		middleware := []middleware{
			0: mw1(trace.ServerMiddleware),
			1: mw3(
				logger.ServerMiddleware, log, logger.MiddlewareConfig{
					Req:     enve.Or(logOr, "PROTOCOL_LOG_LEVEL_HTTP_REQ", slog.LevelDebug),
					Resp2xx: enve.Or(logOr, "PROTOCOL_LOG_LEVEL_HTTP_RESP_2XX", slog.LevelInfo),
					Resp3xx: enve.Or(logOr, "PROTOCOL_LOG_LEVEL_HTTP_RESP_3XX", slog.LevelInfo),
					Resp4xx: enve.Or(logOr, "PROTOCOL_LOG_LEVEL_HTTP_RESP_4XX", slog.LevelWarn),
				},
			),
			2: mw1(panicHandlerMiddleware),
			3: mw1(reqparam.CatchMustMiddleware),
			4: mw1(cors.AllowAll().Handler),
			5: mw3(serverCompressResponsesMiddleware, gzip.DefaultCompression, zstd.SpeedDefault),

			/* TODO: pick values for these and enable them.
			6: mw3(ratelimit.MiddlewareByUser, enve.IntOr("PROTOCOL_HTTP_RATE_LIMIT_PER_USER", 100), time.Tick(enve.DurationOr("PROTOCOL_HTTP_PER_USER_RATE_LIMIT_TICK", time.Minute))),
			7: mw3(ratelimit.MiddlewareByRemoteAddr, enve.IntOr("PROTOCOL_HTTP_RATE_LIMIT_PER_IP", 500), time.Tick(enve.DurationOr("PROTOCOL_HTTP_PER_IP_RATE_LIMIT_TICK", time.Minute))),
			*/
		}
		for i, j := 0, len(middleware)-1; i < j; i, j = i+1, j-1 {
			middleware[i], middleware[j] = middleware[j], middleware[i]
		}

		// apply the middleware in the now-reversed order.
		labels := make([]string, len(middleware))
		for i := range middleware {
			if middleware[i].Fn == nil {
				log.Debug("skipping nil middleware", "index", i, "label", middleware[i].Label)
				labels[i] = "disabled"
			} else {
				h = middleware[i].Fn(h)
				labels[i] = middleware[i].Label
			}
		}
		log.Info(fmt.Sprintf("applied %d middleware", len(labels)), "middleware", labels)
	}
	return h
}

var _ slog.LogValuer

type middleware struct {
	Fn    func(next http.Handler) http.Handler `json:"-"`
	Label string
}

func getfunclabel(v any, args ...any) string {
	f := runtime.FuncForPC(uintptr(reflect.ValueOf(v).UnsafePointer()))
	if f == nil {
		return fmt.Sprintf("unknown %T %v", v, v)
	}
	name := strings.TrimPrefix(f.Name(), "github.com/grene-labs/")
	entry := f.Entry()
	file, line := f.FileLine(0)
	if file == "" || line == 0 {
		file, line = f.FileLine(entry)
	}
	file = strings.TrimPrefix(file, "github.com/grene-labs/grene/")
	var buf strings.Builder
	fmt.Fprintf(&buf, "%s (%#x) @ %s:%d", name, entry, file, line)
	if len(args) == 0 {
		return buf.String()
	}
	fmt.Fprintln(&buf, "args:\n\t")
	for i := range args {
		fmt.Fprintf(&buf, "%d: (%T): %v\n\t", i, args[i], args[i])
	}

	return buf.String()[:len(buf.String())-2] // remove the trailing newline and tab
}

func mw1(f func(next http.Handler) http.Handler) middleware {
	if f == nil {
		panic("middleware function is nil")
	}
	return middleware{Label: getfunclabel(f), Fn: f}
}

/*
// adapt a middleware function with one parameter into a middleware function that takes a single parameter.
func mw2[T0 any](f func(next http.Handler, a T0) http.HandlerFunc, t0 T0) middleware {
	if f == nil {
		panic("middleware function is nil")
	}
	return middleware{
		Label: getfunclabel(f, []any{t0}...),
		// curry the function so that it takes a single parameter.
		Fn: func(next http.Handler) http.HandlerFunc { return f(next, t0) },
	}
}*/

// adapt a middleware function with three parameters into a middleware function that takes a single parameter.
func mw3[T0, T1 any](f func(next http.Handler, a T0, b T1) http.HandlerFunc, t0 T0, t1 T1) middleware {
	return middleware{
		Label: getfunclabel(f, []any{t0, t1}...),
		// curry twice so that it takes a single parameter.
		Fn: func(next http.Handler) http.Handler {
			return f(next, t0, t1)
		},
	}
}

/*
func mw4[T0, T1, T2 any](f func(next http.Handler, a T0, b T1, c T2) http.HandlerFunc, t0 T0, t1 T1, t2 T2) middleware {
	return middleware{
		Label: getfunclabel(f, []any{t0, t1, t2}...),
		// curry 3x so that it takes a single parameter.
		Fn: func(next http.Handler) http.Handler {
			return f(next, t0, t1, t2)
		},
	}
}
*/
