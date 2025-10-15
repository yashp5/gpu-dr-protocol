package unit

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

type test struct {
	name    string
	input   string
	want    float64
	wantErr bool
}

func TestApproxFormatBijection(t *testing.T) {
	n := 1024 * 1024
	if testing.Short() {
		n = 1024
	}
	t.Parallel()
	assertApproxEq := func(t *testing.T, got, want float64) {
		t.Helper()
		tol := max(1e-4, max(math.Abs(want)/1000, math.Abs(got/1000)))
		if math.Abs(got-want) > tol {

			percentOff := 100 * ((got - want) / tol)
			t.Errorf("got %v, want %v +/- %v: %v%% off", got, want, tol, percentOff)
		}
	}

	t.Run("int", func(t *testing.T) {
		t.Parallel()
		tcs := make([]int, n)
		for i := range tcs {
			tcs[i] = rand.Int()
		}

		for i := range tcs {
			s := ApproxFormat(float64(tcs[i]))
			got, err := ParseFloat(s)
			if err != nil {
				t.Errorf("ParseFloat(%q) error = %v", s, err)
			}
			assertApproxEq(t, float64(got), float64(tcs[i]))

		}
	})

	t.Run("float-exp", func(t *testing.T) {
		t.Parallel()
		for i := 0; i < n; i++ {
			sign := 1
			if rand.IntN(2) == 0 {
				sign = -1
			}
			want := rand.ExpFloat64() * float64(sign)
			s := ApproxFormat(want)
			got, err := ParseFloat(s)
			if err != nil {
				t.Errorf("ParseFloat(%q) error = %v", s, err)
			}
			assertApproxEq(t, got, want)
		}
	})
	t.Run("float-uniform", func(t *testing.T) {
		t.Parallel()
		for i := 0; i < n; i++ {
			want := rand.Float64()
			s := ApproxFormat(want)
			got, err := ParseFloat(s)
			if err != nil {
				t.Errorf("ParseFloat(%q) error = %v", s, err)
			}
			assertApproxEq(t, got, want)

		}
	})
}

func TestParse(t *testing.T) {
	t.Parallel()
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()

		tests := []test{
			{"no unit", "1", 1, false},
			{"kB", "1 kB", 1000, false},
			{"MB", "1 MB", 1000000, false},
			{"GB", "1 GB", 1000000000, false},
			{"TB", "1 TB", 1000000000000, false},
			{"PB", "1 PB", 1000000000000000, false},
			{"kB", "1kB", 1000, false},
			{"MB", "1MB", 1000000, false},
			{"GB", "1GB", 1000000000, false},
			{"TB", "1TB", 1000000000000, false},

			// binary
			{"KiB", "1 KiB", 1024, false},
			{"MiB", "1 MiB", 1048576, false},
			{"GiB", "1 GiB", 1073741824, false},
			{"TiB", "1 TiB", 1099511627776, false},
			{"PiB", "1 PiB", 1125899906842624, false},

			// mutiply:
			{"KiB", "5 KiB", 5 * 1024, false},
			{"MiB", "12 MiB", 12 * 1048576, false},
		}

		for _, tt := range tests {
			t.Run("int/"+tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := ParseInt(tt.input)
				if (err != nil) != tt.wantErr {
					t.Errorf("ParseInt(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
					return
				}
				if float64(got) != tt.want {
					t.Errorf("ParseInt(%q) = %v, want %v", tt.input, got, tt.want)
				}
			})
			t.Run("uint/"+tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := ParseUint(tt.input)
				if (err != nil) != tt.wantErr {
					t.Errorf("ParseUint64(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
					return
				}
				if float64(got) != tt.want {
					t.Errorf("ParseUint64(%q) = %v, want %v", tt.input, got, tt.want)
				}
			})
			t.Run("float/"+tt.name, func(t *testing.T) {
				t.Parallel()
				got, err := ParseFloat(tt.input)
				if (err != nil) != tt.wantErr {
					t.Errorf("ParseFloat(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
					return
				}
				if got != tt.want {
					t.Errorf("ParseFloat(%q) = %v, want %v", tt.input, got, tt.want)
				}
			})

		}
	})

	t.Run("error cases", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []test{
			{"empty", "", 0, true},
			{"bad unit", "1 qans,d", 0, true},
			{"no number", "kiB", 0, true},
		} {
			t.Run(tt.name, func(t *testing.T) {
				assertErr(t, ParseInt, tt.input)
				assertErr(t, ParseUint, tt.input)
				assertErr(t, ParseFloat, tt.input)
			})
		}
	})
}

func assertErr[T any](t *testing.T, f func(string) (T, error), key string) {
	t.Helper()
	_, err := f(key)
	if err == nil {
		t.Errorf("expected error: parse %s for %q", reflect.TypeFor[T](), key)
	}
}
