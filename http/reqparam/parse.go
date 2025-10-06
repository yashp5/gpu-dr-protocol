package reqparam

import (
	"strconv"
	"time"
)

// ParseI64 will return int64 from string input.
func ParseI64(inp string) (int64, error) {
	return strconv.ParseInt(inp, 10, 64)
}

func ParseTimeRFC3339(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

// Nop parser that just returns the string as is.
func ParseNop(s string) (string, error) { return s, nil }
