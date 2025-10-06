package reqparam

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
)

// MustQuery is like Query but panics if the value cannot be parsed.
//
// # Warning
//
// Use this with CatchMustMiddleware, which will catch panics and return a 400 Bad Request response.
// It is bad practice
// to panic in production code without a way to handle it gracefully.
func MustQuery[T any](q url.Values, key string, parse func(string) (T, error)) T {
	r, _, err := Query(q, key, parse)
	return must(r, err)
}

// MustQueryOr is like QueryOr but panics if the value cannot be parsed.
// If the key is not present, it returns the default value.
// This will NOT panic for missing keys; it will return the default value.
//
// # Warning
//
// Use this with CatchMustMiddleware, which will catch panics and return a 400 Bad Request response.
func MustQueryOr[T any](q url.Values, key string, parse func(string) (T, error), def T) T {
	val, err := QueryOr(q, key, parse, def)
	if err != nil {
		panic(err)
	}
	return val
}

// QueryOr is like Query but returns a default value if the key is not present. If a value is present but cannot be parsed, it returns an error.
func QueryOr[T any](q url.Values, key string, parse func(string) (T, error), def T) (T, error) {
	val := q.Get(key)
	if val == "" {
		return def, nil
	}
	r, _, err := Query(q, key, parse)
	return r, err
}

// MustQuerySlice is like QuerySlice but panics if the value cannot be parsed.
// This will NOT panic for missing keys; it will return an empty slice.
func MustQuerySlice[T any](q url.Values, key string, parse func(string) (T, error)) ([]T, bool) {
	vals, ok, err := QuerySlice(q, key, parse)
	if err != nil {
		panic(&ParamError{
			Kind:  KindQuery,
			Key:   key,
			Value: vals,
			Type:  reflect.TypeFor[T](),
			Err:   err,
		})
	}
	if !ok {
		panic(&ParamError{
			Kind:  KindQuery,
			Key:   key,
			Value: vals,
			Type:  reflect.TypeFor[T](),
			Err:   ErrNoParam,
		})
	}
	return vals, true
}

// MustQueryJSON is like QueryJSON but panics if the value cannot be parsed.
//
// # Warning
//
// Use this with CatchMustMiddleware, which will catch panics and return a 400 Bad Request response.
// It is bad practice
// to panic in production code without a way to handle it gracefully.
func MustQueryJSON[T any](q url.Values, key string) T { return must(QueryJSON[T](q, key)) }

// QueryJSON is as Query but treats the value as a JSON string and unmarshals it into a type T.
// It returns an error if the value cannot be parsed as JSON or if the type does not match.
// Compare:
//   - Query for a generic value,
//   - MustQuery for a version that panics on error,
//   - MustQueryJSON for a version that panics on error.
func QueryJSON[T any](q url.Values, key string) (T, error) {
	var t T
	val := q.Get(key)
	if val == "" {
		return t, &ParamError{
			Key:   key,
			Value: val,
			Type:  reflect.TypeFor[T](),
			Kind:  KindQuery,
			Err:   ErrNoParam,
		}
	}
	err := json.Unmarshal([]byte(val), &t)
	if err != nil {
		return t, &ParamError{
			Key:   key,
			Value: val,
			Type:  reflect.TypeFor[T](),
			Kind:  KindQuery,
			Err:   err,
		}
	}
	return t, nil
}

// Query extracts a value from the query parameters of an HTTP request, using the provided key.
// Missing or invalid values will return a ParamError.
// Compare:
//
//   - QuerySlice for repeated values,
//   - QueryJSON for JSON values,
//   - MustQuery for a version that panics on error.
func Query[T any](query url.Values, key string, parse func(string) (T, error)) (T, bool, error) {
	ok := query.Has(key)
	if !ok {
		var zero T
		return zero, false, &ParamError{
			Key:   key,
			Value: "",
			Type:  reflect.TypeFor[T](),
			Kind:  KindQuery,
			Err:   ErrNoParam,
		}
	}
	q := query.Get(key)
	result, err := parse(q)
	if err != nil {
		return result, true, &ParamError{
			Key:   key,
			Value: q,
			Type:  reflect.TypeFor[T](),
			Kind:  KindQuery,
			Err:   fmt.Errorf("parse %q: %w", key, errors.Join(err, ErrInvalidParam)),
		}
	}
	return result, true, nil
}

// Query extracts repeated values from the query parameters of an HTTP request using the provided key.
// It is NOT an error if the key is not present - this is an empty slice.
func QuerySlice[T any](q url.Values, key string, parse func(string) (T, error)) ([]T, bool, error) {
	vals := q[key]
	if len(vals) == 0 {
		return nil, false, nil
	}
	results := make([]T, 0, len(vals))
	for _, val := range vals {
		result, err := parse(val)
		if err != nil {
			return nil, true, err
		}
		results = append(results, result)
	}
	return results, true, nil
}
