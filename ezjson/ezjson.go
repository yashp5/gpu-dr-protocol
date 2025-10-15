// Package ezjson provides Unmarshal, which unmarshals JSON into structs with friendlier error messages than the stdlib's encoding/json package and equivalent performance.
// While it has a variety of uses, it was originally designed for friendly web APIs using the JSON over HTTP ("rest").
package ezjson

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"

	"github.com/grene-labs/grene/enve"
)

// levenstein string distance tolerance for help messages. strings more than N apart will be omitted. set by EZJSON_TOL env var at program start.
var TOL atomic.Int64

func init() { TOL.Store(int64(enve.IntOr("EZJSON_TOL", 3))) }

// deref returns the type of v, dereferencing pointers.

// Marshal is just a wrapper for encoding/json.Marshal: see it's docs for details.
func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

var (
	paniconce sync.Once
	panicked  bool
)

// Unmarshal unmarshals JSON into a struct, providing very friendly error messages for mispelled JSON object keys.
// It is functionally equivalent to the following code:
//
//	dec := json.NewDecoder(bytes.NewReader(b))
//	dec.DisallowUnknownFields()
//	return dec.Decode(dst)
func Unmarshal(b []byte, dst any) error {
	// v IS a struct or dereferences to one. unmarshal it, disallowing unknown fields. if and when we hit an unknown field, we will look up the fields of the struct and use them to unmarshal the JSON into v.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()

	err := dec.Decode(dst)
	if err == nil {
		return nil
	}
	if panicked {
		return err
	}
	defer func() {
		// I can't think of any way this could panic, but I refuse to crash someone's program in order to get slightly nicer error messages.
		if p := recover(); p != nil {
			paniconce.Do(
				func() {
					panicked = true
					log.Printf(
						`panic during ezjson.Unmarshal: %v\n%s
THIS SHOULD NEVER HAPPEN.`, p, debug.Stack(),
					)
				},
			)
		}
	}()

	// find the fields of what we were given...
	// This function is a placeholder for the actual unmarshalling logic.
	// It should use the fields and jsonTypes obtained from lookupFields to
	// unmarshal the JSON into the struct v.
	jsonKeys, jsonTypes, _ := keysAndTypes(b)
	structFields, structTypes := lookupFields(deref(reflect.TypeOf(dst)))
	slices.Sort(jsonKeys)

	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 2, 2, 1, ' ', 0)
	write := func(a, b, c, d, e any) {
		fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\n", a, b, c, d, e)
	}
	write("JSON KEY", "STRUCT KEY", "JSON TYPE", "STRUCT TYPE", "KEY DIST")
	write("----", "----", "----", "----", "----")

	var matches []match

	var unknownKeys int
	tol := int(TOL.Load())
	for i := range jsonKeys {
		// any mismatched keys?
		if _, ok := slices.BinarySearch(structFields, jsonKeys[i]); ok { // nope.
			continue
		}
		unknownKeys++
		matches = matches[:0]
		for j := range structFields {
			// if the strings are obviously too dissimilar, don't bother calculating the lev distance
			if minDist := len(jsonKeys[i]) - len(structFields[j]); max(minDist, -minDist) > tol {
				continue
			}
			// they may be within tolerance: find out for sure.
			dist := levdist(jsonKeys[i], structFields[j])
			if dist > tol {
				continue
			}

			matches = append(
				matches, match{
					JSON: entry{
						Key:  jsonKeys[i],
						Type: jsonTypes[i],
					},
					Struct: entry{
						Key:  structFields[j],
						Type: structTypes[j],
					},
					Dist: dist,
				},
			)
		}
		// write in sorted order
		slices.SortFunc(matches, match.Cmp)
		for _, m := range matches {
			write(m.JSON.Key, m.Struct.Key, m.JSON.Type, m.Struct.Type, m.Dist)
		}
	}
	if unknownKeys == 0 { // there WERE no unknown keys: don't bother decorating the error
		return err
	}
	tw.Flush()
	// decorate the error.
	return fmt.Errorf("%w:\nHELP: %d unknown keys\n%s", err, unknownKeys, buf.String())
}

// Get the object keys from a JSON object, recursively.
// That is, KeysAndTypes(`{"obj": {"foo": "foo", "bar": 2}}`) returns ["obj", "obj.foo", "obj.bar"].
// This always does a best-effort search: err != nil does not mean that no keys are found.
// A non-nil error is alw
func keysAndTypes(b []byte) (keys []string, possibleType []string, err error) {
	// if we are here, we have a JSON object. let's get the keys.
	// we will use a recursive function to get all the keys.
	// this is a placeholder for the actual implementation.
	got, types, err := appendKeys(nil, nil, "", b)

	sort.Sort(&s2[string, string]{got, types})
	return got, types, err
}

func guessType(b json.RawMessage) string {
	if len(b) == 0 {
		return "<unknown>"
	}
	if bytes.Equal(b, []byte("null")) {
		return "null"
	}
	switch string(b) {
	case "":
		return "<unknown>"
	case "null":
		return "null"
	case "true", "false":
		return "bool"
	default:
		switch b[0] {
		case '{':
			return "object"
		case '[':
			return "array"
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			return "number"
		case '"':
			return "string"
		default:
			return "unknown"
		}
	}
}

func appendKeys(keys []string, types []string, prefix string, b json.RawMessage) ([]string, []string, error) {
	b = bytes.TrimSpace(b)

	if len(b) == 0 {
		return keys, types, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return keys, types, fmt.Errorf("%s: %w", prefix, err)
	}
	var errs []error
	for k, v := range m {
		var kk string
		if prefix != "" {
			kk = prefix + "." + k
		} else {
			kk = k
		}

		keys = append(keys, kk)
		kt := guessType(v)

		types = append(types, kt)
		if kt == "object" {
			var err error
			keys, types, err = appendKeys(keys, types, kk, v)
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return keys, types, errors.Join(errs...)
}

// ["obj", "obj.foo", "obj.bar"]
// ["struct{Foo string, Bar int}", "string", "int"]
// appendTypeFields(struct{Foo struct{Bar, Baz string} `json:"foo"`}) = appendTypeFields("foo", "struct{Bar, Baz string}"")
func appendTypeFields(prefix string, t reflect.Type, fields, types []string) ([]string, []string) {
	if t.Kind() != reflect.Struct {
		panic(fmt.Errorf("appendTypeFields called with non-struct type: %s", t))
	}

	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() { // skip: empty types or types with '-' json tag
			continue
		}
		tag, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if tag == "-" {
			continue
		}
		if tag == "" {
			tag = sf.Name
		}
		fields = append(fields, prefix+tag)
		types = append(types, sf.Type.String())

		// if this is a primitive, map, array, or slice, we are done: there's nothing more we can do.
		t := deref(sf.Type)
		if t.Kind() != reflect.Struct {
			continue
		}
		// if it's a struct or pointer that eventually dereferences to a struct, we need to recurse.
		raw, loaded := type2fields.Load(t)
		if loaded && (raw).(*cacheEntry).state == stateInProgress {
			continue
		}

		childFields, childTypes := lookupFields(t)
		/*if len(childFields) != len(childTypes) {
					panic(fmt.Errorf(`appendTypeFields(prefix, t, fields, types: mismatch: len(fields) = %d, len(types) = %d
		prefix=%s
		t=%s
		fields=%v
		types=%v`, len(fields), len(types), prefix, t, fields, types))
				}*/
		for i := range childFields {
			fields = append(fields, prefix+tag+"."+childFields[i])
			types = append(types, childTypes[i])
		}

	}

	return fields, types
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

type cacheState byte

const (
	stateUntouched cacheState = iota
	stateInProgress
	stateFinished
)

type cacheEntry struct {
	once   sync.Once
	state  cacheState
	fields []string
	types  []string // types is used for debugging, not for unmarshalling
}

var type2fields sync.Map // map[reflect.Type]*cacheEntry

// get the field names for a struct type, exactly once.
func lookupFields(t reflect.Type) (fields, types []string) {
	raw, ok := type2fields.Load(t) // most of the time this will be a hit, since the number of types in a go program subjected to unmarshalling is small and types are almost never created at runtime.
	if !ok {                       // on the first miss, create a new cache entry...
		raw, _ = type2fields.LoadOrStore(t, new(cacheEntry))
	}

	e := raw.(*cacheEntry)
	e.once.Do(
		func() {
			e.state = stateInProgress
			// preallocate for the common case of small structs
			e.fields, e.types = appendTypeFields(
				"",
				t,
				make([]string, 0, t.NumField()),
				make([]string, 0, t.NumField()),
			)
			sort.Sort(&s2[string, string]{T0: e.fields, T1: e.types})
			e.state = stateFinished
		},
	)
	return e.fields, e.types
}

// implements sort.Sort for two slices on *s2
type s2[T0, T1 cmp.Ordered] struct {
	T0 []T0
	T1 []T1 // types is used for debugging, not for unmarshalling
}

// Less compares elements at i and j in T0 for sorting
func (s *s2[T0, T1]) Less(i, j int) bool {
	return s.T0[i] < s.T0[j]
}

// Len returns the length of the slice
func (s *s2[T0, T1]) Len() int {
	return len(s.T0)
}

// Swap swaps elements at i and j in both T0 and T1
func (s *s2[T0, T1]) Swap(i, j int) {
	s.T0[i], s.T0[j] = s.T0[j], s.T0[i]
	s.T1[i], s.T1[j] = s.T1[j], s.T1[i]
}

func (m match) Cmp(n match) int {
	return cmp.Or(
		m.Dist-n.Dist,
		cmp.Compare(m.JSON.Key, n.JSON.Key),
		cmp.Compare(m.Struct.Key, n.Struct.Key),
	)
}

type entry struct{ Key, Type string }

type match struct {
	JSON, Struct entry
	Dist         int
}
