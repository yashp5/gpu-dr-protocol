package enve

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var namespace int64

func next() int64 { return atomic.AddInt64(&namespace, 1) }

var date = time.Date(2222, 2, 2, 2, 2, 2, 0, time.UTC).Truncate(time.Second)

func TestOr(t *testing.T) {
	t.Parallel()
	testOr(t, map[string]bool{"TRUE": true, "true": true, "false": false, "FALSE": false, "1": true, "0": false}, BoolOr)
	testOr(t, map[string]string{"foo": "foo"}, StringOr)
	testOr(t, map[string]time.Duration{"10m": 10 * time.Minute, "1h30s": time.Hour + 30*time.Second}, DurationOr)
	testOr(t, map[string]time.Time{date.Format(time.RFC3339): date}, TimeRFC3339Or)
}

func TestMust(t *testing.T) {
	t.Parallel()
	testMust(t, map[string]bool{"TRUE": true, "true": true, "false": false, "FALSE": false, "1": true, "0": false}, MustBool)
	testMust(t, map[string]string{"foo": "foo"}, MustString)
	testMust(t, map[string]time.Duration{"10m": 10 * time.Minute, "1h30s": time.Hour + 30*time.Second}, MustDuration)
	testMust(t, map[string]int{"2": 2, "-1": -1, "+1": 1}, MustInt)
	testMust(t, map[string]uint64{"2": 2, "1": 1}, MustUint64)
	testMust(t, map[string]time.Time{date.Format(time.RFC3339): date}, MustTimeRFC3339)
}

func testOr[T comparable](t *testing.T, cases map[string]T, f func(string, T) T) {
	var zero T
	for s, want := range cases {
		s, want := s, want
		key := fmt.Sprintf("test_lookup_%T_%04d", zero, next())
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			t.Run("fallback when missing", func(t *testing.T) {
				os.Unsetenv(key)

				if got := f(key, want); got != want {
					t.Errorf("fallback on missing key %s: expected %+v, but got %+v", key, want, got)
				}

				if got := f(key, zero); got != zero {
					t.Errorf("fallback on missing key %s: expected %+v, but got %+v", key, zero, got)
				}
			})
			t.Run("fallback on bad value", func(t *testing.T) {
				if reflect.TypeOf(zero) == reflect.TypeOf("") {
					t.Skipf("string parsing cannot fail")
				}
				os.Setenv(key, "🔥")
				if got := f(key, want); got != want {
					t.Errorf("expected %+v, but got %+v", want, got)
				}
			})
			t.Run("happy path", func(t *testing.T) {
				os.Setenv(key, s)
				if got := f(key, zero); got != want {
					t.Errorf("expected %+v, but got %+v", want, got)
				}
			})
		})
	}
}

// we don't want the compiler to optimize things out, so let's have global side effects.
var v interface{}

func testMust[T comparable](t *testing.T, cases map[string]T, f func(key string) T) {
	var zero T
	key := strings.ToUpper(fmt.Sprintf("TEST_LOOKUP_%T_%04d", zero, next()))

	for s, want := range cases {
		want := want
		t.Run(s, func(t *testing.T) {
			t.Run("panics on missing key", func(t *testing.T) {
				os.Unsetenv(key)
				defer func() {
					if p := recover(); p == nil {
						t.Fatal("expected a panic, but got none")
					}
				}()
				v = f(key)
			})
			t.Run("panics on invalid value", func(t *testing.T) {
				if reflect.TypeOf(zero) == reflect.TypeOf("") {
					t.Skipf("string parsing cannot fail")
				}
				os.Setenv(key, "🔥")
				defer func() {
					if p := recover(); p == nil {
						t.Error("expected a panic, but got none")
					}
				}()
				v = f(key)
			})
			t.Run("happy path", func(t *testing.T) {
				t.Logf("setting environment variable %q to %q", key, s)
				os.Setenv(key, s)
				if got := f(key); got != want {
					t.Errorf("expected to parse  %v as %v, but got %v", s, want, got)
				}
			})
		})
	}
}
