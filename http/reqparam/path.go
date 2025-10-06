package reqparam

import (
	"encoding/json"
	"net/http"
	"reflect"
)

// MustPath is as Path but panics if the value cannot be parsed.
// Unlike the other Must* functions, this is usually the correct function to use - a missing path parameter is a programmer error.
func MustPath[T any](r *http.Request, key string, parse func(string) (T, error)) T {
	return must(Path(r, key, parse))
}

// MustPathJSON is like MustPath but parses the value as JSON into a type T.
// Warning: this is intended to be used with the CatchMustMiddleware, which will catch panics and return a 400 Bad Request response; it is bad practice
// to panic in production code without a way to handle it gracefully.
func MustPathJSON[T any](r *http.Request, key string) T { return must(PathJSON[T](r, key)) }

// PathJSON is as Path but parses the value as JSON into a type T.
func PathJSON[T any](r *http.Request, key string) (T, error) {
	var t T
	raw := r.PathValue(key)
	if raw == "" {
		type_ := reflect.TypeOf(t)
		return t, &ParamError{
			Kind:  KindPath,
			Key:   key,
			Value: raw,
			Type:  type_,
			Err:   ErrNoParam,
		}
	}

	err := json.Unmarshal([]byte(raw), &t)
	if err != nil {
		type_ := reflect.TypeOf(t)
		return t, &ParamError{
			Kind:  KindPath,
			Key:   key,
			Value: raw,
			Type:  type_,
			Err:   err,
		}
	}
	return t, nil
}

// Path extracts a value from the path of an HTTP request, using the provided key.
// Usually, you'll want to use one of the specialized functions like PathUUID, PathString, or PathInt.
// The parse function should convert the string value to the desired type T, returning an error if parsing fails.
func Path[T any](r *http.Request, key string, parse func(string) (T, error)) (t T, err error) {
	raw := r.PathValue(key)
	if raw == "" {
		type_ := reflect.TypeOf(t)
		return t, &ParamError{
			Kind:  KindPath,
			Key:   key,
			Value: raw,
			Type:  type_,
			Err:   ErrNoParam,
		}
	}

	t, err = parse(raw)
	if err != nil {
		type_ := reflect.TypeOf(t)
		return t, &ParamError{
			Kind:  KindPath,
			Key:   key,
			Value: raw,
			Type:  type_,
			Err:   err,
		}
	}
	return t, nil
}
