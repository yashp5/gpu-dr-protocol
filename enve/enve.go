package enve

import (
	"fmt"
	"log"
	"log/slog"
	"os"
)

type missingKeyError string

func (err missingKeyError) Error() string {
	return fmt.Sprintf("missing required envvar %s", string(err))
}

// Lookup and parse the specified environment variable key, returning ErrorMissingKey if it's missing, and the result of parse(os.Getenv(key)) otherwise.
// The error message will include the key.
func Lookup[T any](parse func(s string) (T, error), key string) (T, error) {
	s, ok := os.LookupEnv(key)
	if !ok {
		return *new(T), missingKeyError(key)
	}
	return parse(s)
}

// Or looks up & parses the specified environment variable key.
// A missing or unparsable value will return the backup value, logging the error (if any) and the fallback valuue.
// Successful output will be logged exactly once if ENVE_LOG_SUCCESS is true.
func Or[T any](parse func(string) (T, error), key string, backup T) T {
	t, err := Lookup(parse, key)
	if !shouldLog(key) {
		if err != nil {
			return backup
		}
		return t
	}

	if flag&logSlog != 0 {
		log := slog.Default().With(
			slog.Group("enve",
				slog.String("key", key),
				slog.String("caller", callerMetadata().String()),
				slog.Any("default", backup),
				slog.String("parser", parserMetadata(parse).String())),
		)

		// slog logging mode using log/slog
		if err != nil {
			if _, ok := err.(missingKeyError); ok {
				log.Info("enve: missing optional envvar: falling back to default")
				return backup
			}
			log.Info("enve: invalid optional envvar: falling back to default")
			return backup
		}
		if flag&logSuccess != 0 {
			log.Info("enve: ok")
		}
		return t
	}

	// regular logging mode using "log"

	if err != nil {
		// no need to tell people about the parser: that's not the problem.
		if _, ok := err.(missingKeyError); ok {
			log.Printf("enve: missing optional envvar %s: falling back to default: %v; caller %v", key, backup, callerMetadata())
			return backup
		}
		log.Printf("enve: invalid optional envvar %s: %v: falling back to default  %v; caller %s; parser %s", key, err, backup, callerMetadata(), parserMetadata(parse))
		return backup
	}
	if flag&logSuccess != 0 {
		log.Printf("enve: envvar %s: ok", key)
	}
	return t
}

// Must looks up & parses the specified environment variable key, panicking on a missing value or parse error.
// Successful output will be logged exactly once if ENVE_LOG_SUCCESS is true.
func Must[T any](parse func(string) (T, error), key string) T {
	t, err := Lookup(parse, key)
	if err != nil {
		if flag&logSlog != 0 {
			slogger := slog.Default().With(slog.Group("enve", slog.String("key", key), slog.String("caller", callerMetadata().String())))
			if _, ok := err.(missingKeyError); ok {
				slogger.Error("enve: missing required envvar")
				panic("enve: missing required envvar " + key)
			}
			slogger.Error("enve: parsing required envvar", slog.String("error", err.Error()))
			panic(fmt.Errorf("enve: parsing required envvar %s into type %s: %w", key, typeOf[T](), err))
		}
		if _, ok := err.(missingKeyError); ok {
			log.Panicf("enve: %s FATAL ERR: missing required envvar %s", callerMetadata(), key)
		}
		log.Printf("enve: %s parser error: %v", parserMetadata(parse), err)
		log.Panicf("enve: %s FATAL ERR: parsing required envvar %s into type %s: %v", callerMetadata(), key, typeOf[T](), err)
	}
	if flag&logSuccess == 0 {
		return t
	}
	if flag&logSlog != 0 {
		slogger := slog.Default().With(slog.Group("enve", slog.String("key", key), slog.String("caller", callerMetadata().String()), slog.String("parser", parserMetadata(parse).String())))
		slogger.Info("envvar " + key + ": ok")
		return t
	}
	log.Printf("enve: envvar %s: ok", key)

	return t
}
