package ezjson

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

var f, t []string

func TestJSONKeys(t *testing.T) {
	t.Parallel()
	type recursive struct {
		Baz       string     `json:"baz"`
		Qux       int        `json:"qux"`
		Recursive *recursive `json:"recursive"`
	}
	type complex struct {
		Foo    string `json:"foo"`
		Bar    int    `json:"bar"`
		NoTag  string
		Nested recursive `json:"nested"`
		OmitMe string    `json:"-"`
	}
	b, _ := json.Marshal(complex{
		Foo:   "foo",
		Bar:   2,
		NoTag: "notag",
		Nested: recursive{
			Baz:       "baz",
			Qux:       5,
			Recursive: &recursive{Baz: "recursive l2"},
		},
	})
	got, _, err := keysAndTypes(b)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields("NoTag bar foo nested nested.baz nested.qux nested.recursive nested.recursive.baz nested.recursive.qux nested.recursive.recursive")
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func BenchmarkLookupFields(b *testing.B) {
	type recursive struct {
		Baz       string     `json:"baz"`
		Qux       int        `json:"qux"`
		Recursive *recursive `json:"recursive"`
	}
	type complex struct {
		Foo    string `json:"foo"`
		Bar    int    `json:"bar"`
		NoTag  string
		Nested recursive `json:"nested"`
		OmitMe string    `json:"-"`
	}
	for _, type_ := range []reflect.Type{
		reflect.TypeFor[complex](),
		reflect.TypeFor[recursive](),
		reflect.TypeFor[struct {
			a, b, c struct{ foo, bar, baz string }
		}](),
		reflect.TypeFor[struct{ foo, bar, baz string }](),
	} {
		b.Run(type_.Name(), func(b *testing.B) {
			b.Run("cached", func(b *testing.B) {
				for b.Loop() {
					f, t = lookupFields(type_)
				}
			})

			b.Run("uncached", func(b *testing.B) {
				for b.Loop() {
					type2fields = sync.Map{}
					f, t = lookupFields(type_)
				}
			})
		})
	}
}

func TestLookupFields(t *testing.T) {
	type N struct {
		Baz       string `json:"baz"`
		Qux       int    `json:"qux"`
		Recursive *N     `json:"recursive"`
	}
	type S struct {
		Foo    string `json:"foo"`
		Bar    int    `json:"bar"`
		NoTag  string
		Nested N      `json:"nested"`
		OmitMe string `json:"-"`
	}
	fields, types := lookupFields(reflect.TypeOf(S{}))
	/*for i := range fields {
		t.Logf("%s: %s", fields[i], types[i])
	}*/
	want := [][2]string{
		{"NoTag", "string"},
		{"bar", "int"},
		{"foo", "string"},
		{"nested", "ezjson.N"},
		{"nested.baz", "string"},
		{"nested.qux", "int"},
		{"nested.recursive", "*ezjson.N"},
	}
	for i := range want {
		if i >= len(fields) {
			t.Errorf("expected %s, but got nothing", want[i])
			continue
		}
		if fields[i] != want[i][0] {
			t.Errorf("expected field %d to be %s, but got %s", i, want[i][0], fields[i])
		}
		if types[i] != want[i][1] {
			t.Errorf("expected type %d to be %s, but got %s", i, want[i][1], types[i])
		}

	}
}
