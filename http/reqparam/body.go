package reqparam

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"gitlab.com/efronlicht/ezjson"
)

// KeyJSONBody is a constant used to represent the JSON body in error messages.
// It is used as a key in ParamError to indicate that the error is related to the
// JSON body of the request rather than a specific parameter key.
const KeyJSONBody = "<json body>"

// JSONBody decodes a JSON body from an io.ReadCloser into a type T, closing the reader afterwards.
// It returns an error if the body cannot be decoded or if the type does not match.
// Errors will return a ParamError with Kind set to KindBody, Key set to "body", Value as
// and Type set to the type of T.
func JSONBody[T any](rc io.ReadCloser) (t T, err error) {
	if rc == nil {
		return t, &ParamError{
			Kind:  KindBody,
			Key:   KeyJSONBody,
			Value: json.RawMessage(nil),
			Err:   fmt.Errorf("%w: no body", ErrNoParam),
			Type:  reflect.TypeFor[T](),
		}
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		return t, errors.Join(err, rc.Close())
	}
	if err := ezjson.Unmarshal(b, &t); err != nil {
		return t, &ParamError{
			Kind:  KindBody,
			Key:   KeyJSONBody,
			Err:   err,
			Value: json.RawMessage(b),
			Type:  reflect.TypeFor[T](),
		}
	}

	return t, rc.Close()
}

// MustJSONBody is like JSONBody but panics if the body cannot be decoded.
//
// # Warning
//
// This is intended to be used with the CatchMustMiddleware, which will catch panics and return a 400 Bad Request response; it is bad practice
// to panic in production code without a way to handle it gracefully.
func MustJSONBody[T any](rc io.ReadCloser) T {
	t, err := JSONBody[T](rc)
	if err != nil {
		panic(err)
	}
	return t
}
