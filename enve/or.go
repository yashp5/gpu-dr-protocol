package enve

import (
	"encoding"
	"errors"
	"strconv"
	"time"

	"github.com/grene-labs/grene/unit"
)

// BoolOr returns the boolean value represented by the environment variable.
// It accepts 1, t, T, TRUE, true, True, 0, f, F, FALSE, false, False. Any other value will return backup.
func BoolOr(key string, backup bool) bool { return Or(strconv.ParseBool, key, backup) }

func notEmpty(s string) (string, error) {
	if s == "" {
		return "", errors.New("unexpected empty string")
	}
	return s, nil
}

// StringOr returns os.Getenv(key) if it's not the empty string, Or backup otherwise.
func StringOr(key, backup string) string {
	return Or(notEmpty, key, backup)
}

// FromTextOr looks up and parses the envvar as a TextUnmarshaler.
func FromTextOr[T any, PT interface {
	*T
	encoding.TextUnmarshaler
}](
	key string, backup T,
) T {
	return Or(parseFromText[T, PT], key, backup)
}

func parseFromText[T any, PT interface {
	*T
	encoding.TextUnmarshaler
}](
	s string,
) (T, error) {
	t := PT(new(T))
	err := t.UnmarshalText([]byte(s))
	return *t, err
}

// DurationOr looks up and parses the envvar as a duration.
func DurationOr(key string, backup time.Duration) time.Duration {
	return Or(time.ParseDuration, key, backup)
}

// TimeRFC3339Or wraps [Or](parse.TimeRFC3339, key, backup)
func TimeRFC3339Or(key string, backup time.Time) time.Time {
	return Or(timeRFC3339, key, backup)
}

// IntOr wraps Or(strconv.ParseInt(key, 0, 0), key, backup)
func IntOr(key string, backup int) int {
	return Or(atoi, key, backup)
}

// Uint64Or wraps Or(parse.Uint, key, backup)
func Uint64Or(key string, backup uint64) uint64 {
	return Or(atou, key, backup)
}

// FloatOr wraps Or(parse.Float, key, backup)
func FloatOr(key string, backup float64) float64 {
	return Or(atof, key, backup)
}

// UnitIntOr is as IntOr but with an optional unit suffix.
//
//	os.Setenv("FOO", "1 KiB")
//	fmt.Println(UnitIntOr("FOO", 0) )
//
// Output:
//
// 1024
func IntUnitOr(key string, backup int) int {
	return Or(unit.ParseInt, key, backup)
}

// UnitUintOr is as UintOr but with an optional unit suffix.
//
//	os.Setenv("FOO", "1 KiB")
//	fmt.Println(UnitUintOr("FOO", 0) )
//
// Output:
// 1024
func UintUnitOr(key string, backup uint) uint {
	return Or(unit.ParseUint, key, backup)
}

// UnitFloatOr is as FloatOr but with an optional unit suffix.
//
//	os.Setenv("FOO", "1 KiB")
//	fmt.Println(UnitFloatOr("FOO", 0) )
func FloatUnitOr(key string, backup float64) float64 {
	return Or(unit.ParseFloat, key, backup)
}
