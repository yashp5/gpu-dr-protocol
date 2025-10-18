// Package dbutil provides utility functions for database operations,
// mostly converting types to to and from pgtype/v5 types.
package dbutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/govalues/decimal"
	"github.com/jackc/pgx/v5/pgtype"
)

// IntTNumeric converts an int to a pgtype.Numeric, saving allocations if possible.
// Compare Float2Numeric.
func Int2Numeric[N interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}](src N) pgtype.Numeric {
	initSmallNumerics()
	// fast path: lookup small numerics
	if n, ok := f2n[float64(src)]; ok {
		return n
	}
	// slow path: allocate a new numeric, including the *big.Int.

	return pgtype.Numeric{
		Int:   big.NewInt(int64(src)),
		Valid: true,
	}
}

// lookup table of small numeric values to avoid allocations in common cases.
// this is built exactly once on the first call to Float2Numeric or Int2Numeric,
// so as not to waste time or memory if it's not used.
var (
	smallNumOnce sync.Once
	// to pgtype.Numeric

	d2n map[decimal.Decimal]pgtype.Numeric
	f2n map[float64]pgtype.Numeric
)

// build a table of small numeric values to avoid allocations in common cases.
// this holds:
// -128 to 127
// 0 to 1 in 0.01 increments
// positive and negative infinity
// powers of 10
func initSmallNumerics() {
	smallNumOnce.Do(
		func() {
			start := time.Now()
			f2n = make(map[float64]pgtype.Numeric, 256+151)
			f2n[0.0] = pgtype.Numeric{
				// zero
				Int:   big.NewInt(0),
				Valid: true,
			}
			f2n[math.Inf(1)] = pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}
			f2n[math.Inf(-1)] = pgtype.Numeric{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
			f2n[math.NaN()] = pgtype.Numeric{NaN: true, Valid: true}

			for x := float64(-128); x <= 127; x++ {
				f2n[x] = pgtype.Numeric{
					Int:   big.NewInt(int64(x)),
					Valid: true,
				}
			}
			// small numerics from -1 to 1 in 0.01 increments
			for x := -1.0; x <= 1.0; x += 0.01 {
				s := strconv.FormatFloat(x, 'f', -1, 64)
				var n pgtype.Numeric
				if err := n.Scan(s); err != nil {
					panic(fmt.Sprintf("failed to convert %f to pgtype.Numeric: %v", x, err))
				}
				f2n[x] = n
			}
			// powers of 10 up to 10^16
			for i := 0; i <= 16; i++ {
				f := math.Pow(10, float64(i))
				s := strconv.FormatFloat(f, 'f', -1, 64)
				var n pgtype.Numeric
				if err := n.Scan(s); err != nil {
					panic(fmt.Sprintf("failed to convert %f to pgtype.Numeric: %v", f, err))
				}
				f2n[f] = n
			}

			// initialize d2n
			d2n = make(map[decimal.Decimal]pgtype.Numeric, len(f2n))

			for k, n := range f2n {
				d, err := decimal.NewFromFloat64(k) // convert float64 to decimal.Decimal
				if err == nil {                     // decimal values are not always valid, e.g. NaN or Infinity; so skip those
					d2n[d] = n
				}
			}

			slog.Default().Debug(
				"initialized small numerics",
				"count", len(f2n),
				"duration", time.Since(start),
			)
		},
	)
}

// Float2Numeric converts a float64 to a pgtype.Numeric, saving allocations if possible.
// This always succeeds, even for NaN and Infinity.
// Compare Int2Numeric.
func Float2Numeric(x float64) pgtype.Numeric {
	initSmallNumerics()
	// fast path: lookup small numerics
	if n, ok := f2n[x]; ok {
		return n
	}
	switch {
	case math.IsNaN(x):
		return pgtype.Numeric{NaN: true, Valid: true} // NaN is a special case
	case math.IsInf(x, 1):
		return pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true} // positive infinity
	case math.IsInf(x, -1):
		return pgtype.Numeric{InfinityModifier: pgtype.NegativeInfinity, Valid: true} // negative infinity
	default:
		// slow path: allocate a new numeric, including the *big.Int.
		s := strconv.FormatFloat(x, 'f', -1, 64)
		var n pgtype.Numeric

		if err := n.Scan(s); err != nil { // this should be impossible, but let's handle it gracefully
			panic(fmt.Sprintf("%T -> %T: %v", x, n, err))
		}
		return n

	}
}

// Numeric2Float converts a pgtype.Numeric to a float64, or returns an error if the numeric is invalid, NaN, or infinity.
func Numeric2Float(n pgtype.Numeric) (float64, error) {
	initSmallNumerics()
	v, err := n.Float64Value()
	return v.Float64, err
}

// MustNumeric2Float is as Numeric2Float, but panics on error.
func MustNumeric2Float(n pgtype.Numeric) float64 {
	f, err := Numeric2Float(n)
	if err != nil {
		panic(fmt.Sprintf("failed to convert pgtype.Numeric to float64: %v", err))
	}
	return f
}

// ParseUUIDBytes parses a UUID from a byte slice. It expects the UUID to be exactly 28 hexadecimal characters, with optional dashes, spaces, underlines, or other whitespace.
// It doesn't care about capitalization.
func ParseUUIDBytes(src []byte) (pgtype.UUID, error) {
	got, err := uuid.ParseBytes(src)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("failed to parse UUID %q: %w", src, err)
	}
	return UUIDToDB(got), nil
}

// errors returned by MustNumeric2Decimal.
var (
	ErrInvalidNumeric = fmt.Errorf("invalid pgtype.Numeric")
	ErrInfinity       = fmt.Errorf("pgtype.Numeric is infinity or negative infinity")
	ErrNaN            = fmt.Errorf("pgtype.Numeric is NaN")
	ErrEmptyInput     = fmt.Errorf("empty input")
)

// As Decimal2Numeric, but panics on error.
func MustDecimal2Numeric(n decimal.Decimal) pgtype.Numeric {
	d, err := Decimal2Numeric(n)
	if err != nil {
		panic(fmt.Sprintf("failed to convert decimal.Decimal to pgtype.Numeric: %v", err))
	}
	return d
}

func Numeric2Int[T interface{ ~uint64 | ~int | ~int64 }](n pgtype.Numeric) (t T, err error) {
	switch { // fast path: special cases for invalid & NaN values, and fast paths for int64 and uint64.
	case !n.Valid:
		return t, ErrInvalidNumeric
	case n.NaN:
		return t, ErrNaN
	case n.InfinityModifier == pgtype.Infinity, n.InfinityModifier == pgtype.NegativeInfinity:
		return t, ErrInfinity
	case n.Int.Sign() == 0:
		return t, nil // fast path: zero is always zero, no matter the exponent
	default:

		// slow path: go through JSON. this is slow, but reliable.
		// TODO: profile this, as it seems like a likely performance bottleneck.

		// we can't use an ordinary cache here like in Decimal2Numeric because
		// the underlying *big.Int is a POINTER. we could do unsafe fiddling to cache, but let's demonstrate
		// a need before we go crazy.
		b, err := n.MarshalJSON()
		if err != nil {
			return t, fmt.Errorf("failed to marshal pgtype.Numeric to JSON: %w", err)
		}
		err = json.Unmarshal(b, &t)
		return t, err
	}
}

// Convert a numeric value to an integer, or panic on error.
func MustNumeric2Int[T interface {
	~uint64 |
		~int |
		~int64
}](n pgtype.Numeric) T {
	t, err := Numeric2Int[T](n)
	if err != nil {
		panic(err)
	}
	return t
}

func Decimal2Numeric(d decimal.Decimal) (n pgtype.Numeric, err error) {
	initSmallNumerics()
	if n, ok := d2n[d]; ok { // fast path: lookup small decimals
		return n, nil
	}
	// slow path: go through Scan. slow, but reliable. we should revisit this later; it's likely to be a performance bottleneck.
	if err := n.Scan(d.String()); err != nil {
		return n, fmt.Errorf("scan decimal string to pgtype.Numeric: %w", err)
	}
	return n, nil
}

// As Numeric2DecimalTrunc, but panics on error.
func MustNumeric2DecimalTrunc(n pgtype.Numeric) decimal.Decimal {
	d, err := Numeric2DecimalTrunc(n)
	if err != nil {
		panic(fmt.Sprintf("failed to convert pgtype.Numeric to decimal.Decimal: %v", err))
	}
	return d
}

// As MustNumeric2Decimal, truncating the decimal part to 19 digits of combined precision.
// (That is, we ONLY truncate the decimal part, not the integer part, to 19 digits of precision,
// not counting the .)
func Numeric2DecimalTrunc(n pgtype.Numeric) (d decimal.Decimal, err error) {
	switch { // fast path: special cases for invalid & NaN values, and fast paths for int64 and uint64.
	case !n.Valid:
		return d, ErrInvalidNumeric
	case n.NaN:
		return d, ErrNaN
	case n.InfinityModifier == pgtype.Infinity, n.InfinityModifier == pgtype.NegativeInfinity:
		return d, ErrInfinity
	case n.Int.Sign() == 0:
		return decimal.Zero, nil // fast path: zero is always zero, no matter the exponent
	default:

		// slow path: go through JSON. this is slow, but reliable.
		// TODO: profile this, as it seems like a likely performance bottleneck.

		// we can't use an ordinary cache here like in Decimal2Numeric because
		// the underlying *big.Int is a POINTER. we could do unsafe fiddling to cache, but let's demonstrate
		// a need before we go crazy.
		b, err := n.MarshalJSON()
		if err != nil {
			return d, fmt.Errorf("failed to marshal pgtype.Numeric to JSON: %w", err)
		}
		if len(b) > 19 {
			before, after, ok := bytes.Cut(b, []byte("."))

			if len(before) > 19 {
				return d, fmt.Errorf("fractional part of %s has %d digits, but max is 19", b, len(before))
			}
			if ok {
				after = after[:19-len(before)]
				b = fmt.Appendf(b[:0], "%s.%s", before, after)
			}

		}
		if err := d.UnmarshalJSON(b); err != nil {
			return d, fmt.Errorf("%w: %s", err, b)
		}
		return d, nil
	}
}

// Parse a UUID from a string into a pgtype.UUID.
func ParseUUID(src string) (pgtype.UUID, error) {
	got, err := uuid.Parse(src)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("failed to parse UUID %q: %w", src, err)
	}
	return UUIDToDB(got), nil
}

// UUIDToDB converts a uuid.UUID to a pgtype.UUID.
func UUIDToDB(src uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: src,
		Valid: true,
	}
}

// UUIDV7 generates a new UUIDv7 and returns it as a pgtype.UUID.
func NewUUIDv7() pgtype.UUID {
	b, err := uuid.NewV7()
	if err != nil {
		panic(err) // this should be impossible: it means the system random number generator is broken
	}
	var idv7 pgtype.UUID
	idv7.Bytes = b
	idv7.Valid = true
	return idv7
}

func ToNullablePgTimestamp(t *time.Time) pgtype.Timestamp {
	if t == nil || t.IsZero() {
		return pgtype.Timestamp{Valid: false}
	}
	return pgtype.Timestamp{
		Time:  *t,
		Valid: true,
	}
}

func ToNullablePgUUID() pgtype.UUID {
	return pgtype.UUID{Valid: false}
}

// DBToUUID convert pgtype.UUID to uuid.
func DBToUUID(src pgtype.UUID) uuid.UUID {
	b, err := src.UUIDValue()
	if err != nil {
		panic(err)
	}
	return b.Bytes
}

func MicrosecondsToDuration(microseconds int64) time.Duration {
	return time.Duration(microseconds) * time.Microsecond
}
