// Package unit provides functions to parse and format numbers with optional unit suffixes.
package unit

import (
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

// IEC Binary units: KiB=1024, MiB=1024², GiB=1024³, TiB=1024⁴, PiB=1024⁵
// This package prefers binary units.
const (
	KiB = 1 << 10
	MiB = 1 << 20
	GiB = 1 << 30
	TiB = 1 << 40
	PiB = 1 << 50
)

// SI base-10 units: K=1e3, M=1e6, G=1e9, T=1e12, P=1e15, E=1e18.
const (
	K = 1000
	M = 1000 * 1000
	G = 1000 * 1000 * 1000
	T = 1000 * 1000 * 1000 * 1000
	P = 1000 * 1000 * 1000 * 1000 * 1000
)

func splitUnit(s string) (num string, mul uint64, err error) {
	var x, suffix string
	for i := len(s) - 1; i >= 0; i-- { // start from the end and consume everything until we hit a digit
		if s[i] >= '0' && s[i] <= '9' {
			x = strings.TrimSpace(s[:i+1])
			suffix = strings.TrimSpace(s[i+1:])
			goto FOUND
		}
	}
	return s, 1, nil // it's all digits
FOUND: // is this a unit suffix?
	switch strings.ToLower(suffix) {
	case "", "b":
		mul = 1
	case "k", "kb":
		mul = K
	case "m", "mb":
		mul = M
	case "g", "gb":
		mul = G
	case "t", "tb":
		mul = T
	case "p", "pb":
		mul = P

	case "ki", "kib":
		mul = KiB
	case "mi", "mib":
		mul = MiB
	case "gi", "gib":
		mul = GiB
	case "ti", "tib":
		mul = TiB
	case "pi", "pib":
		mul = PiB

	default:
		return "", 0, fmt.Errorf("unexpected unit suffix %q", suffix)
	}
	return x, mul, nil
}

// ParseUint parses a string with an optional unit suffix like "KiB" or "kB" as a uint.
//
//	fmt.Println(unit.ParseUint("1 KiB")) // 1024
//	fmt.Println(unit.ParseUint("1"))     // 1
func ParseUint(s string) (uint, error) {
	n, err := ParseUint64(s)
	if n > math.MaxUint { // nop on 64-bit machines
		return 0, fmt.Errorf("out of range: %d", n)
	}
	return uint(n), err
}

func boundsCheck(a, b uint64) error {
	if hi, _ := bits.Mul64(a, b); hi != 0 {
		return fmt.Errorf("%#x*%#x+ overflows uint64", a, b)
	}
	return nil
}

// Number is a type that can be (lossily) converted to a float64.
type Number interface {
	~float64 | ~int64 | ~uint64 | ~int | ~uint | ~uint32 | ~int32 | ~int16 | ~uint16 | ~int8 | ~uint8 | ~uintptr
}

// ApproxFormat returns a human-readable approximation of x with three significant digits.
// Handy for stuff like setting disk quotas, etc.
func ApproxFormat[N Number](n N) string {
	x := float64(n)
	switch {
	case x < KiB:
		return fmt.Sprintf("%3.4g B", x)
	case x < MiB:
		return fmt.Sprintf("%3.4g KiB", x/float64(KiB))
	case x < GiB:
		return fmt.Sprintf("%3.4g MiB", x/float64(MiB))
	case x < TiB:
		return fmt.Sprintf("%3.4g GiB", x/float64(GiB))
	case x < PiB:
		return fmt.Sprintf("%3.4g TiB", x/float64(TiB))
	default:
		return fmt.Sprintf("%3.4g PiB", x/float64(PiB))
	}
}

// ParseFloat parses a string with an optional unit suffix like "KiB" or "kB" as a float64.
//
//	x, _ := ParseFloat("1.8MiB")
//	fmt.Printf("%.3f\n", x)
//
// Output:
//
// 1887436.800
func ParseFloat(s string) (float64, error) {
	s, mul, err := splitUnit(s)
	if err != nil {
		return 0, err
	}
	x, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0, fmt.Errorf("invalid float %q", s)
	}
	return x * float64(mul), nil
}

// ParseUint64 parses a string with an optional unit suffix like "KiB" or "kB" as a uint64.
//
//	fmt.Println(unit.ParseUint64("1 KiB")) // 1024
func ParseUint64(s string) (uint64, error) {
	s, mul, err := splitUnit(s)
	if err != nil {
		return 0, err
	}
	x, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		return 0, err
	}

	return x * mul, boundsCheck(x, mul)
}

// ParseInt parses a string with an optional unit suffix like "KiB" or "kB" as an integer.
// The integer part is parsed as though with strconv.ParseInt(s, 0, 64).
// This function raises an error if the result would be out of range of an int.
//
//	fmt.Println(unit.ParseInt("1 KiB")) // 1024
func ParseInt(s string) (int, error) {
	n, err := ParseInt64(s)
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt || n < math.MinInt { // nop on 64-bit machines
		return 0, fmt.Errorf("out of range: %d", n)
	}
	return int(n), err
}

// ParseInt64 parses a string with an optional unit suffix like "KiB" or "kB" as an int64.
// The integer part is parsed as though with strconv.ParseInt(s, 0, 64).
// This function raises an error if the result would be out of range of an int64.
//
//	fmt.Println(unit.ParseInt64("1 KiB"))
//
// Output:
//
// 1024
func ParseInt64(s string) (int64, error) {
	s, mul, err := splitUnit(s)
	if err != nil {
		return 0, err
	}
	x, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return 0, err
	}
	n := x * int64(mul)
	if (x >= 0) != (n >= 0) {
		return n, fmt.Errorf("overflow: %d*%d", x, mul)
	}

	return n, boundsCheck(uint64(x), mul)
}
