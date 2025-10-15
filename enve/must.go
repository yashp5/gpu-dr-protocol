// Package enve retrieves & parses environment variables with optional logging.
//   - Handy logs & error messages so you can easily discover your configuration by just running the program.
//   - Lets you know by default what knobs are required or missing... at most once per environment variable.
//   - As simple an API as I could make.
package enve

import (
	"strconv"
	"time"
)

// MustString looks up the envvar, panicking on a missing value. Unlike "Or", the empty string is OK.
func MustString(key string) string { return Must(nop, key) }

// MustBool wraps Must(strconv.ParseBool, key).
func MustBool(key string) bool { return Must(strconv.ParseBool, key) }

// MustDuration wraps Must(time.ParseDuration, key)
func MustDuration(key string) time.Duration { return Must(time.ParseDuration, key) }

// MustTimeRFC3339 wraps Must(parse.TimeRFC3339, key)
func MustTimeRFC3339(key string) time.Time { return Must(timeRFC3339, key) }

// MustUint64 wraps Must(strconv.ParseUint(key, 0, 64), key)
func MustUint64(key string) uint64 { return Must(atou, key) }

// MustInt wraps Must(strconv.ParseInt(key, 0, 0), key)
func MustInt(key string) int { return Must(atoi, key) }

// MustFloat wraps Must(strconv.ParseFloat(key, 64), key)
func MustFloat(key string) float64 { return Must(atof, key) }
