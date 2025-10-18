// count.go contains the Caller type and the count struct, which counts calls by caller.
// this is used by LogDB and logtx to count calls by caller.
package dbutil

import (
	"cmp"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unique"

	"github.com/grene-labs/grene/http/writeresp"
)

// Caller of the database and the SQL statement, if any.
// Used by the 'count' field of LogDB to count calls by caller.
type Caller struct {
	Fileline, Func, SQL string
}

// Count of calls by caller.
type Count struct {
	m sync.Map
}

func (c *Count) inc(caller Caller) {
	v, loaded := c.m.Load(caller)
	if !loaded {
		v, _ = c.m.LoadOrStore(caller, &atomic.Int64{})
	}
	v.(*atomic.Int64).Add(1)
}

func (c *Count) Iter() iter.Seq2[Caller, int64] {
	return func(yield func(Caller, int64) bool) {
		c.m.Range(func(key, value any) bool {
			yield(key.(Caller), value.(*atomic.Int64).Load())
			return true
		})
	}
}

type CountDumpResponse struct {
	Fileline string  `json:"fileline"`
	Func     string  `json:"func"`
	SQL      string  `json:"sql"`
	Count    int64   `json:"count"`
	Percent  float64 `json:"%,omitempty"` // proportion of total calls.
}

func (c *Count) DumpMostCommonFirst() []CountDumpResponse {
	var dst []CountDumpResponse
	var total int64
	for k, v := range c.Iter() {
		dst = append(dst, CountDumpResponse{
			Fileline: k.Fileline,
			Func:     k.Func,
			SQL:      k.SQL,
			Count:    v,
		})
		total += v
	}
	dst = append(dst, CountDumpResponse{
		Fileline: "ALL",
		Func:     "ALL",
		SQL:      "ALL",
		Count:    total,
	})
	slices.SortFunc(dst, func(a, b CountDumpResponse) int {
		return -cmp.Compare(a.Count, b.Count) // descending
	})
	if total == 0 {
		return dst
	}

	for i := range dst {
		// calculate proportion as a percentage with 1 decimal place.
		// e.g. 12.3%
		dst[i].Percent = (float64(dst[i].Count) / float64(total)) * 100
	}

	return dst
}

func (c *Count) BySQL() map[string]int64 {
	dst := make(map[string]int64)
	var total int64
	for k, v := range c.Iter() {
		dst[k.SQL] += v
		total += v
	}
	dst["ALL"] = total
	return dst
}

func (c *Count) ByFileLine() map[string]int64 {
	dst := make(map[string]int64)
	var total int64
	for k, v := range c.Iter() {
		dst[k.Fileline] += v
		total += v
	}
	dst["ALL"] = total
	return dst
}

func (c *Count) ByFunc() map[string]int64 {
	var total int64
	dst := make(map[string]int64)
	for k, v := range c.Iter() {
		dst[k.Func] += v
		total += v
	}
	dst["ALL"] = total
	return dst
}

// GET /v1/debug/db/count/caller
// unlike the other endpoints, this one returns a slice of objects, not a map,
// since JSON can't have object keys that are not strings.
func (c *Count) ByCallerHandler(w http.ResponseWriter, r *http.Request) {
	if c == nil {
		writeresp.JSONErr(r.Context(), w, fmt.Errorf("count is nil"), http.StatusServiceUnavailable)
		return
	}
	writeresp.JSONData(r.Context(), w, c.DumpMostCommonFirst(), 200)
}

// GET /v1/debug/db/count/sql
func (c *Count) BySQLHandler(w http.ResponseWriter, r *http.Request) {
	if c == nil {
		writeresp.JSONErr(r.Context(), w, fmt.Errorf("count is nil"), http.StatusServiceUnavailable)
		return
	}

	writeresp.JSONData(r.Context(), w, c.BySQL(), 200)
}

// GET /v1/debug/db/count/fileline
func (c *Count) ByFileLineHandler(w http.ResponseWriter, r *http.Request) {
	if c == nil {
		writeresp.JSONErr(r.Context(), w, fmt.Errorf("count is nil"), http.StatusServiceUnavailable)
		return
	}

	writeresp.JSONData(r.Context(), w, c.ByFileLine(), 200)
}

// GET /v1/debug/db/count/func
func (c *Count) ByFuncHandler(w http.ResponseWriter, r *http.Request) {
	if c == nil {
		writeresp.JSONErr(r.Context(), w, fmt.Errorf("count is nil"), http.StatusServiceUnavailable)
		return
	}

	writeresp.JSONData(r.Context(), w, c.ByFunc(), 200)
}

func caller(c *Count, sql string) (caller Caller) {
	callers := make([]uintptr, 6)
	n := runtime.Callers(2, callers)
	frames := runtime.CallersFrames(callers[:n])

	for {
		frame, ok := frames.Next()
		if !ok {
			break
		}
		if strings.HasPrefix(frame.Function, "github.com/dora-network/dora/dbutil.") {
			continue // skip dbutil package functions.
		}
		if strings.HasPrefix(frame.Function, "github.com/jackc/pgx/v5.") {
			continue // skip pgx package functions.
		}
		if strings.Contains(frame.Function, "generated/sqlc.") {
			continue // skip sqlc package functions.
		}
		caller.Fileline = fmt.Sprintf("%s:%d", frame.File, frame.Line)
		if _, after, ok := strings.Cut(caller.Fileline, "dora-network/"); ok {
			caller.Fileline = after // trim the dora prefix.
		}
		caller.Fileline = unique.Make(caller.Fileline).Value() // since we just created this string, make sure it's unique so as to not have huge memory usage storing different copies of the same string.
		caller.Func = frame.Function
		caller.SQL = sql
		if caller.Func == "" {
			caller.Func = "unknown"
		}
		// trim the package path from the function name.
		if _, after, ok := strings.Cut(caller.Func, "dora-network/"); ok {
			caller.Func = after
		}
		c.inc(caller)
		return caller
	}
	caller.Fileline = "unknown:0"
	caller.Func = "unknown"
	return caller
}

var _ slog.LogValuer = Caller{}

func (c Caller) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("fileline", c.Fileline),
		slog.String("func", c.Func),
		slog.String("sql", c.SQL),
	)
}
