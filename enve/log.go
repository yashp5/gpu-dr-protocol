package enve

import (
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const (
	logDisabled = 0b000
	logSuccess  = 0b001 // if ON, log successes. if OFF, don't.
	logError    = 0b010 // if ON, log errors. if OFF, don't.
	logSlog     = 0b100 // if ON, use slog. if OFF, use log.
)

type logFlag uint8

var (
	seen = make(map[string]bool)
	flag logFlag
	mux  sync.RWMutex // guards seen
	once sync.Once    // guards flag and seen
)

const (
	// environment variables to control logging behavior.
	ENVE_LOG_SUCCESS  = "ENVE_LOG_SUCCESS"  // if false, don't log successes (found/parsed key for the first time)
	ENVE_LOG_ERROR    = "ENVE_LOG_ERROR"    // if false, don't log errors (missing keys, bad values)
	ENVE_LOG_SLOG     = "ENVE_LOG_SLOG"     // environment variable to enable "log/slog" logging mode; if false, use "log" instead.
	ENVE_LOG_DISABLED = "ENVE_LOG_DISABLED" // environment variable to disable logging entirely; overrides all other logging settings.
)

func shouldLog(key string) bool {
	once.Do(func() { flag = parseFlags() }) // once ever
	if flag == logDisabled {
		return false
	}
	// check once: has anyone else logged this?
	mux.RLock()
	ok := seen[key]
	mux.RUnlock()
	if ok { //  they have.
		return false
	}
	// obtain the write lock.
	mux.Lock()
	defer mux.Unlock()
	// someone else might have got the write lock between when we released the read lock and obtained the write lock.
	if seen[key] { // they have; we have nothing to do.
		return false
	}
	// we're the first to obtain the write lock.
	seen[key] = true
	return true
}

func parseFlags() logFlag {
	disabled, _ := strconv.ParseBool(os.Getenv(ENVE_LOG_DISABLED))
	if disabled {
		return logDisabled
	}
	flag |= logError

	if ok, _ := strconv.ParseBool(os.Getenv(ENVE_LOG_SLOG)); ok {
		flag |= logSlog
	}

	if ok, _ := strconv.ParseBool(os.Getenv(ENVE_LOG_SUCCESS)); ok {
		flag |= logSuccess
	}
	return flag
}

type metadata struct {
	name, file string
	line       int
}

func (m metadata) String() string {
	return fmt.Sprintf("%s:%d %s", m.file, m.line, m.name)
}

func callerMetadata() metadata {
	pc, callerFile, callerLine, _ := runtime.Caller(skip)
	return metadata{
		name: trim(runtime.FuncForPC(pc).Name()),
		file: trim(callerFile),
		line: callerLine,
	}
}

// trims a function path containing "grene/enve", making it start with "enve".
// EG, "users/grene/go/src/github.com/grene/enve/parse.Duration" => "enve/parse.Duration".
func trim(s string) string {
	if _, after, ok := strings.Cut(s, "grene/"); ok && strings.HasPrefix(after, "enve") {
		return after
	}
	return s
}

func parserMetadata[T any](f func(string) (T, error)) metadata {
	meta := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	parserFile, parserLine := meta.FileLine(meta.Entry())
	return metadata{
		name: trim(meta.Name()),
		file: trim(parserFile),
		line: parserLine,
	}
}

// how far up the stack to go to get caller information.
// supose we call env.DurationOr("CLIENT_TIMEOUT", 5*time.Second) from main.main().
// stack should look like this:
//
//	 callerMeta() // 0
//		log.Or[time.Duration]("CLIENT_TIMEOUT", ErrMissingKey("CLIENT_TIMEOUT", parse.Duration, 5*time.Second)) // 1
//		env.or[time.Duration](parse.Duration, "CLIENT_TIMEOUT", 5*time.Second) // 2
//		env.DurationOr("CLIENT_TIMEOUT", ErrMissingKey("CLIENT_TIMEOUT")) // 3
//		main.main() // 4 <<- this is what we want
const skip = 3

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }
