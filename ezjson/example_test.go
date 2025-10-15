package ezjson_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/grene-labs/grene/ezjson"
)

// Example HTTP server using ezjson for friendlier errors.
// In this example, our customer has provided a field named "color" when they should have provided a field named "colors".
func Example_httpServer() {
	handler := func(w http.ResponseWriter, r *http.Request) {
		type User struct {
			First  string   `json:"first"`
			Middle string   `json:"middle"`
			Last   string   `json:"last"`
			Colors []string `json:"colors"`
		}
		var u User
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if err := ezjson.Unmarshal(b, &u); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(u); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	req, _ := http.NewRequest(
		"POST",
		srv.URL+"/user",
		strings.NewReader(`{"first":"John","middle":"H","last":"Doe","color":["blue","green"]}`),
	) // "colors" is mis-spelled
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("%s\n\n%s\n", resp.Status, b)

	// Output:
	// 400 Bad Request
	//
	// json: unknown field "color":
	// HELP: 1 unknown keys
	// JSON KEY STRUCT KEY JSON TYPE STRUCT TYPE KEY DIST
	// ----     ----       ----      ----        ----
	// color    colors     array     []string    1
}

// Example of how ezjson works with various fields
func Example_fields() {
	var s struct { // same rules as the encoding/json package
		foo  string // unexported fields are ignored
		Food string // exported fields use the struct name...
		Fop  int

		NNNNN_ struct{} `json:"moo"` // unless there's a JSON tag, which uses that
		Fo     string   `json:"-"`   // the special tag '-' means ignore this key even though it's exported
	}
	err := ezjson.Unmarshal([]byte(`{"foo": "bar"}`), &s)
	fmt.Println(err.Error())
	// Output:
	// json: unknown field "foo":
	// HELP: 1 unknown keys
	// JSON KEY STRUCT KEY JSON TYPE STRUCT TYPE KEY DIST
	// ----     ----       ----      ----        ----
	// foo      moo        string    struct {}   1
	// foo      Food       string    string      2
	// foo      Fop        string    int         2
}

// Example of how ezjson handles nested JSON objects
func Example_nestedJSONObjects() {
	type Nested struct {
		A     string `json:"a"`
		Inner struct {
			Foo, Bar string
		} `json:"inner"`
	}

	b := []byte(`{
		"a": "a", 
		"inner": {"Baz": "baz", "Foo": "foo"}
	}`)
	var dst Nested
	err := ezjson.Unmarshal(b, &dst)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	// Output:
	//	json: unknown field "Baz":
	// HELP: 1 unknown keys
	// JSON KEY  STRUCT KEY JSON TYPE STRUCT TYPE KEY DIST
	// ----      ----       ----      ----        ----
	// inner.Baz inner.Bar  string    string      1
	// inner.Baz inner.Foo  string    string      3
}
