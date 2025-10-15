package ezjson

// code below adapted from  https://github.com/agnivade/levenshtein under the following license, retrieved 2025-08-21.
// I got rid of the part where it's UTF-8 sensitive, because I care more about allocations than localization.
// You may feel differently: feel free to fork and sub it out.

/*
	The MIT License (MIT)

# Copyright (c) 2015 Agniva De Sarker

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

// calculate levenstein distance
func levdist(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}

	if len(b) == 0 {
		return len(a)
	}

	if a == b {
		return 0
	}

	// swap to save some memory O(min(a,b)) instead of O(a)
	if len(a) > len(b) {
		a, b = b, a
	}
	// remove trailing identical byte seqs
	a, b = trimLCS(a, b)

	// Remove leading identical bytes
	a, b = trimLCP(a, b)

	// we can skip an alloc if both strings are ASCII.
	for i := range a {
		if a[i]&0b1000_0000 != 0 {
			return utf8dist([]rune(a), []rune(b))
		}
	}
	for i := range b {
		if b[i]&0b1000_0000 != 0 {
			return utf8dist([]rune(a), []rune(b))
		}
	}

	return asciidist(a, b)
}

// levenstein distance for ASCII strings where len(a) <= len(b)
func asciidist(a, b string) int {
	lena := len(a)
	lenb := len(b)

	// Init the row.
	var x []uint16
	const N = 32 // BUFSIZE

	if lena+1 > N {
		x = make([]uint16, lena+1)
	} else {
		// We make a small optimization here for small strings.
		// Because a slice of constant length is effectively an array,
		// it does not allocate. So we can re-slice it to the right length
		// as long as it is below a desired threshold.
		x = make([]uint16, N)
		x = x[:lena+1]
	}

	// we start from 1 because index 0 is already 0.
	for i := 1; i < len(x); i++ {
		x[i] = uint16(i)
	}

	// hoist bounds checks out of the loops
	_ = x[lena]
	y := x[1:]
	y = y[:lena]
	// fill in the rest
	for i := 0; i < lenb; i++ {
		prev := uint16(i + 1)
		for j := 0; j < lena; j++ {
			current := x[j] // match
			if b[i] != a[j] {
				current = min(x[j], prev, y[j]) + 1
			}
			x[j] = prev
			prev = current
		}
		x[lena] = prev
	}
	return int(x[lena])
}

// levenstein distance for possibly non-ASCII strings where len(a) <= len(b)
func utf8dist(a, b []rune) int {
	lena := len(a)
	lenb := len(b)

	// Init the row.
	var x []uint16
	const N = 32
	if lena+1 > N {
		x = make([]uint16, lena+1)
	} else {
		// We make a small optimization here for small strings.
		// Because a slice of constant length is effectively an array,
		// it does not allocate. So we can re-slice it to the right length
		// as long as it is below a desired threshold.
		x = make([]uint16, N)
		x = x[:lena+1]
	}

	// we start from 1 because index 0 is already 0.
	for i := 1; i < len(x); i++ {
		x[i] = uint16(i)
	}

	// hoist bounds checks out of the loops
	_ = x[lena]
	y := x[1:]
	y = y[:lena]
	// fill in the rest
	for i := 0; i < lenb; i++ {
		prev := uint16(i + 1)
		for j := 0; j < lena; j++ {
			current := x[j] // match
			if b[i] != a[j] {
				current = min(x[j], prev, y[j]) + 1
			}
			x[j] = prev
			prev = current
		}
		x[lena] = prev
	}
	return int(x[lena])
}

func trimLCS(a, b string) (string, string) {
	m := min(len(a), len(b))
	a2 := a[len(a)-m:]
	b2 := b[len(b)-m:]
	i := len(a2)
	b2 = b2[:i] // hoist bounds checks out of the loop
	for ; i > 0 && a2[i-1] == b2[i-1]; i-- {
		// deliberately empty body
	}
	return a[:len(a)-len(a2)+i], b[:len(b)-len(b2)+i]
}

func trimLCP(a, b string) (string, string) {
	var i int
	for m := min(len(a), len(b)); i < m && a[i] == b[i]; i++ {
		// deliberately empty body
	}
	return a[i:], b[i:]
}
