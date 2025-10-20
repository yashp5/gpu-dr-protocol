// TODO: implement cache
// Package cache contains an in-memory, expiring cache.
// It is used to cache JSON responses, but can be used for any type of data.
package cache

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Create a new cache with the given interval, cutoff age, and target size.
// Cleanup will run every interval, deleting entries older than cutoff age.
// If the cache is larger than target size, it will delete the least recently used entries until it is smaller than target size.
func New[K comparable, V any](getSize func(V) int, interval, cutoffAge time.Duration, targetSize int) (*Cache[K, V], context.CancelFunc) {
	gc := &Cache[K, V]{}
	ctx, cancel := context.WithCancel(context.Background())
	go gc.startCleanup(ctx, getSize, interval, cutoffAge, targetSize)
	return gc, cancel
}

// NewResponseCache creates a new cache for JSON responses.
func NewJSONResponse(interval, cutoffAge time.Duration, targetSize int) (*JSONResponse, context.CancelFunc) {
	getSize := func(v json.RawMessage) int { return len(v) }
	return New[string](getSize, interval, cutoffAge, targetSize)
}

type (
	// In-memory cache from K to V.
	Cache[K comparable, V any] struct{ m sync.Map }
	// ResponseCache is a type alias for a cache of JSON responses.
	JSONResponse    = Cache[string, json.RawMessage]
	cacheVal[V any] struct {
		v                    V
		storedAt, lastAccess time.Time
	}
)

type kv[K comparable, V any] struct {
	k K
	v *cacheVal[V]
}

// start cleanup on the cache every interval.
// cleanup works as follows:
// - any entry older than cutoff is deleted without further checks.
// - after this initial round, if the cache is larger than targetSize, we delete the least recently used entries until the cache is smaller than targetSize.
// this function only returns when the context is cancelled, so run it in a goroutine.
func (c *Cache[K, V]) startCleanup(ctx context.Context, getSize func(V) int, interval, cutoff time.Duration, targetSize int) {
	ticker := time.Tick(interval)

	var live, removed []kv[K, V]
	for {
		select {
		case <-ticker:
			live, removed = docleanup(getSize, live[:0], removed[:0], c, time.Now().Add(-cutoff), targetSize)
			// TODO: collect stats on live and removed in a more robust way.
		case <-ctx.Done():
			return
		}
	}
}

func docleanup[K comparable, V any](
	getSize func(v V) int,
	entrybuf, removedbuf []kv[K, V], cache *Cache[K, V], cutoff time.Time, targetSize int,
) (possiblyLive, removed []kv[K, V]) {
	possiblyLive = entrybuf[:0]
	removed = removedbuf[:0]
	cache.m.Range(func(rawk, rawv any) bool {
		k := rawk.(K)
		v := rawv.(*cacheVal[V])
		if v.storedAt.Before(cutoff) {
			// try to delete the entry. there's a chance someone else is warming the cache at the same time, so use CompareAndDelete.

			if cache.m.CompareAndDelete(k, v) { // we deleted it.
				removed = append(removed, kv[K, V]{
					k: k,
					v: v,
				})
				return true
			}
			// someone else touched the cache. leave it for next round.
			return true
		}
		// mark this entry as live; we'll check if we're overweight in a second.
		possiblyLive = append(possiblyLive, kv[K, V]{
			k: k,
			v: v,
		})
		return true
	})

	var used int
	for i := range possiblyLive {
		used += getSize(possiblyLive[i].v.v)
	}
	if used < targetSize {
		return // no need to clean up
	}

	slices.SortFunc(possiblyLive, func(a, b kv[K, V]) int {
		return a.v.lastAccess.Compare(b.v.lastAccess) // least recently used first.
	})
	live := possiblyLive[:0]
	var i int
	for i = range possiblyLive {
		if used < targetSize {
			break
		}
		k := possiblyLive[i].k
		v := possiblyLive[i].v
		if cache.m.CompareAndDelete(k, v) { // we deleted it.
			removed = append(removed, kv[K, V]{
				k: k,
				v: v,
			})
			used -= getSize(v.v)

			removed = append(removed, kv[K, V]{
				k: k,
				v: v,
			})
		} else {
			// someone else touched the cache. leave it for next round.
			live = append(live, kv[K, V]{
				k: k,
				v: v,
			})
		}
	}
	live = append(live, possiblyLive[i:]...) // copy over the rest of the entries; they're definitely live for this round.

	return live, removed
}

// Load returns the cached value for the given key, if it exists, updating the last access time.
// It returns the value, the time it was stored, and a boolean indicating whether the value was found.
// If the value is not found, it returns nil, zero time, and false.
func (c *Cache[K, V]) Load(key string) (V, time.Time, bool) {
	rawv, ok := c.m.Load(key)
	var v V
	if !ok {
		return v, time.Time{}, false
	}
	old := rawv.(*cacheVal[V])
	updated := &cacheVal[V]{
		v:          old.v,
		storedAt:   old.storedAt,
		lastAccess: time.Now(),
	}
	_ = c.m.CompareAndSwap(key, old, updated) // try to update the last access time. if we fail, we don't care; that means someone else has deleted or updated it.

	return old.v, old.storedAt, true
}

type cachingResponseWriter struct {
	http.ResponseWriter
	cache  *Cache[string, json.RawMessage]
	status int
	b      []byte
}

// Write the response and store it in teh cache.
func (w *cachingResponseWriter) Write(b []byte) (int, error) {
	if w.cache == nil {
		return w.ResponseWriter.Write(b)
	}
	w.b = append(w.b, b...)
	return w.ResponseWriter.Write(b)
}

func (c *Cache[K, V]) Store(k K, v V) {
	now := time.Now()
	c.m.Store(k, &cacheVal[V]{
		v:          v,
		storedAt:   now,
		lastAccess: now,
	})
}

var maxAgeRegexp = regexp.MustCompile(`(?i)max-age=(\d+)`)

func Middleware(c *JSONResponse, ttl time.Duration, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "" {
			http.Error(w, "path not found", http.StatusNotFound)
			return
		}
		// only cache GET and HEAD requests.
		// if the request is not a GET, just call the next handler.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		// Cache-Control:

		control := r.Header.Get("Cache-Control")
		if control == "" {
			goto NO_CONTROL
		}
		// check the Cache-Control header for anything that indicates we should not cache the response.
		for _, v := range []string{"no-store", "no-cache", "private", "max-age=0"} {
			if strings.Contains(control, v) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if match := maxAgeRegexp.FindStringSubmatch(control); match != nil {
			// check the max-age directive. if it's 0, we should not cache the response.
			rawAge, err := strconv.Atoi(match[1])
			if err != nil || rawAge == 0 {
				// assume no cache.
				next.ServeHTTP(w, r)
				return
			}
			ttl = min(time.Duration(rawAge)*time.Second, ttl)
		}
	NO_CONTROL:
		// check the cache.
		cached, storedAt, ok := c.Load(path)

		// cache hit. use the cached response rather than calling the handler.
		if ok && time.Since(storedAt) < ttl {
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("X-Cache-Stored-At", storedAt.Format(time.RFC3339))
			w.Header().Set("Content-Type", "application/json")
			w.Write(cached)
			return
		}

		// cache miss. wrap the response writer to capture the response so we can cache it if it's a 2xx response.
		cw := &cachingResponseWriter{
			ResponseWriter: w,
			cache:          c,
		}

		next.ServeHTTP(cw, r) // allow the handler to run.

		// check the status code. if it's a 2xx response, cache it.
		if cw.status >= 200 && cw.status < 300 {
			c.Store(path, cw.b)
		}
	}
}
