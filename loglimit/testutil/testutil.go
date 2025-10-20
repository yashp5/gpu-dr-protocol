package testutil

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// send n logs to the logger in the form LEVEL_%03d
//
//	logN(log, slog.LevelDebug, 10) // logs DEBUG_000 through DEBUG_009
func LogN(log *slog.Logger, level slog.Level, n int) {
	for i := 0; i < n; i++ {
		log.Log(context.TODO(), level, fmt.Sprintf("%s_%03d", level, i))
	}
}

// BuildPanicAssert builds a test helper that asserts that a function panics.
func BuildPanicAssert(t *testing.T, f any) func(label string, args ...any) {
	t.Helper()

	type_ := reflect.TypeOf(f)
	if type_.Kind() != reflect.Func {
		t.Fatalf("expected a function, got %T", f)
	}
	if reflect.TypeOf(f).Kind() != reflect.Func {
		t.Fatalf("expected a function, got %T", f)
	}
	numIn := reflect.TypeOf(f).NumIn()
	inTypes := make([]reflect.Type, numIn)
	for i := 0; i < numIn; i++ {
		inTypes[i] = type_.In(i)
	}
	val := reflect.ValueOf(f)
	vargs := make([]reflect.Value, numIn)

	return func(label string, args ...any) {
		t.Helper()
		t.Run(
			label, func(t *testing.T) {
				t.Helper()
				defer func() {
					if recover() == nil {
						t.Errorf("expected a panic")
					}
				}()
				if len(args) != numIn {
					t.Fatalf("expected %d arguments, got %d", numIn, len(args))
				}
				clear(vargs)
				for i, a := range args {
					// interesting reflect gotcha:
					// if we pass a "typed nil" into a function that expects an any (that is, interface{}),
					// we lose the type information. we then have an untyped nil: we need it to be a typed nil again.
					// we can fix this by converting it to the expected type.
					vargs[i] = reflect.ValueOf(a).Convert(inTypes[i])
				}
				defer func() {
					if p := recover(); p != nil && strings.Contains(fmt.Sprint(p), "reflect") {
						t.Logf("%#+v, %#+v", f, args)
						t.Errorf("reflect panic: %v", p)
					} else {
						panic(p)
					}
				}()
				val.Call(vargs)
			},
		)
	}
}

func AssertContains(t *testing.T, s string, want ...string) {
	t.Helper()
	var missing []string
	for _, v := range want {
		if !strings.Contains(s, v) {
			missing = append(missing, v)
		}
	}
	slices.Sort(missing)
	missing = slices.Compact(missing)
	if len(missing) > 0 {
		t.Errorf("missing %q", missing)
		t.Log("got: ", s)
	}
}

func AssertOmits(t *testing.T, s string, dontWant ...string) bool {
	t.Helper()
	var present []string
	for _, v := range dontWant {
		if strings.Contains(s, v) {
			present = append(present, v)
		}
	}
	slices.Sort(present)
	present = slices.Compact(present)
	if len(present) > 0 {
		t.Errorf("expected to omit %q", present)
	}
	return true
}

func TestHandler(t *testing.T) (*strings.Builder, slog.Handler) {
	var b strings.Builder

	var endbuf strings.Builder
	t.Helper()
	t.Cleanup(
		func() {
			t.Helper()
			if t.Failed() {
				t.Logf("all log output:\n%s", strings.ReplaceAll(b.String(), "msg=", ""))
			}
		},
	)

	out := io.MultiWriter(&b, &endbuf)
	h := slog.NewTextHandler(
		out, &slog.HandlerOptions{
			AddSource: false, Level: slog.LevelDebug, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Key == "time" {
					return slog.Attr{}
				}
				if a.Key == "level" {
					return slog.Attr{}
				}
				return a
			},
		},
	)
	return &b, h
}
