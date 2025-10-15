package ezjson_test

import (
	"strings"
	"testing"

	"github.com/grene-labs/grene/ezjson"
)

func TestUnmarshal(t *testing.T) {
	t.Parallel()
	err := ezjson.Unmarshal(
		[]byte(`{"foo": "bar"}`), new(
			struct {
				Food                       string
				Fop                        int
				CheckJSONTag               struct{} `json:"moo"`
				ignoreUnexported           int
				AboveTestDistanceThreshold string
			},
		),
	)
	if err == nil {
		t.Fatalf("expected an error, but got nil")
	}

	msg := err.Error()
	var missing []string
	for _, v := range strings.Fields("        foo      moo        string    struct {}   1") {
		if !strings.Contains(msg, v) {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		t.Errorf("err message %v missing expected substrings %v", msg, missing)
	}
}

type Match struct {
	S string // string
	N int    // distance
}

func (m Match) Cmp(p Match) int {
	if m.N < p.N {
		return -1
	} else if m.N > p.N {
		return 1
	}
	return strings.Compare(m.S, p.S)
}
