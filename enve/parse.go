package enve

import (
	"strconv"
	"time"
)

const (
	MiB = 1 << 20
)

// nop does nothing.
func nop(s string) (string, error) { return s, nil }

// time.Parse(time.RFC3339, s)
func timeRFC3339(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err
}

// atou, atof, atoi are aliases for strconv.ParseUint, strconv.ParseFloat, and strconv.Atoi.
func atou(s string) (uint64, error) { return strconv.ParseUint(s, 0, 64) }

// atou, atof, atoi are type aliases for strconv.ParseUint, strconv.ParseFloat, and strconv.Atoi.
func atof(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

// atou, atof, atoi are type aliases for strconv.ParseUint, strconv.ParseFloat, and strconv.Atoi.
func atoi(s string) (int, error) { return strconv.Atoi(s) }
