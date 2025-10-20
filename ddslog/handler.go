package ddslog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"github.com/grene-labs/grene/ddslog/internal/buffer"
)

func (h *DDHandler) clone() *DDHandler {
	// We can't use assignment because we can't copy the mutex.
	h2 := *h
	h2.preformattedAttrs = slices.Clip(h.preformattedAttrs)
	h2.groups = slices.Clip(h.groups)
	return &h2
}

// enabled reports whether l is greater than or equal to the
// minimum level.
func (h *DDHandler) enabled(l slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.opts.Level != nil {
		minLevel = h.opts.Level.Level()
	}
	return l >= minLevel
}

// countEmptyGroups returns the number of empty group values in its argument.
func countEmptyGroups(as []slog.Attr) int {
	n := 0
	for _, a := range as {
		if a.Value.Kind() == slog.KindGroup && len(a.Value.Group()) == 0 {
			n++
		}
	}
	return n
}

func (h *DDHandler) withAttrs(as []slog.Attr) *DDHandler {
	// We are going to ignore empty groups, so if the entire slice consists of
	// them, there is nothing to do.
	if countEmptyGroups(as) == len(as) {
		return h
	}
	h2 := h.clone()
	// Pre-format the attributes as an optimization.
	state := h2.newHandleState((*buffer.Buffer)(&h2.preformattedAttrs), false, "")
	defer state.free()
	state.prefix.WriteString(h.groupPrefix)
	if pfa := h2.preformattedAttrs; len(pfa) > 0 {
		state.sep = h.attrSep()
		if pfa[len(pfa)-1] == '{' {
			state.sep = ""
		}
	}
	// Remember the position in the buffer, in case all attrs are empty.
	pos := state.buf.Len()
	state.openGroups()
	if !state.appendAttrs(as) {
		state.buf.SetLen(pos)
	} else {
		// Remember the new prefix for later keys.
		h2.groupPrefix = state.prefix.String()
		// Remember how many opened groups are in preformattedAttrs,
		// so we don't open them again when we handle a Record.
		h2.nOpenGroups = len(h2.groups)
	}
	return h2
}

func (h *DDHandler) withGroup(name string) *DDHandler {
	h2 := h.clone()
	h2.groups = append(h2.groups, name)
	return h2
}

// handle is the internal implementation of Handler.Handle
// used by TextHandler and JSONHandler.
func (h *DDHandler) handle(ctx context.Context, r slog.Record) error {
	state := h.newHandleState(buffer.New(), true, "")
	defer state.free()

	state.buf.WriteByte('{')

	// Built-in attributes. They are not in a group.
	stateGroups := state.groups
	state.groups = nil // So ReplaceAttrs sees no groups instead of the pre groups.
	rep := h.opts.ReplaceAttr
	// time
	if !r.Time.IsZero() {
		key := slog.TimeKey
		val := r.Time.Round(0) // strip monotonic to match slog.Attr behavior
		if rep == nil {
			state.appendKey(key)
			appendJSONTime(&state, val)
		} else {
			state.appendAttr(slog.Time(key, val))
		}
	}
	// level
	key := slog.LevelKey
	val := r.Level
	if rep == nil {
		state.appendKey(key)
		state.appendString(val.String())
	} else {
		state.appendAttr(slog.Any(key, val))
	}
	// source
	if h.opts.AddSource && r.PC != 0 {

		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		state.appendAttr(
			slog.Any(
				slog.SourceKey, &slog.Source{
					Function: f.Function,
					File:     f.File,
					Line:     f.Line,
				},
			),
		)
	}
	key = slog.MessageKey
	msg := r.Message
	if rep == nil {
		state.appendKey(key)
		state.appendString(msg)
	} else {
		state.appendAttr(slog.String(key, msg))
	}
	state.groups = stateGroups // Restore groups passed to ReplaceAttrs.
	state.appendNonBuiltIns(r)
	state.buf.WriteByte('\n')

	b := json.RawMessage(bytes.Clone(*state.buf))
	go func() {
		h.mux.Lock()
		defer h.mux.Unlock()
		h.w.Write(b)
	}()

	select {
	case h.formatted <- json.RawMessage(b):
		// Successfully sent the record.
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default: // too much data: drop the record
		return fmt.Errorf("cannot send log: channel full to cap %d", cap(h.batched))
	}
}

func (s *handleState) appendNonBuiltIns(r slog.Record) {
	// preformatted Attrs
	if pfa := s.h.preformattedAttrs; len(pfa) > 0 {
		s.buf.WriteString(s.sep)
		s.buf.Write(pfa)
		s.sep = ","
		if pfa[len(pfa)-1] == '{' {
			s.sep = ""
		}
	}
	// Attrs in Record -- unlike the built-in ones, they are in groups started
	// from WithGroup.
	// If the record has no Attrs, don't output any groups.
	nOpenGroups := s.h.nOpenGroups
	if r.NumAttrs() > 0 {
		s.prefix.WriteString(s.h.groupPrefix)
		// The group may turn out to be empty even though it has attrs (for
		// example, ReplaceAttr may delete all the attrs).
		// So remember where we are in the buffer, to restore the position
		// later if necessary.
		pos := s.buf.Len()
		s.openGroups()
		nOpenGroups = len(s.h.groups)
		empty := true
		r.Attrs(
			func(a slog.Attr) bool {
				if s.appendAttr(a) {
					empty = false
				}
				return true
			},
		)
		if empty {
			s.buf.SetLen(pos)
			nOpenGroups = s.h.nOpenGroups
		}
	}

	// Close all open groups.
	for range s.h.groups[:nOpenGroups] {
		s.buf.WriteByte('}')
	}
	// Close the top-level object.
	s.buf.WriteByte('}')
}

// attrSep returns the separator between attributes.
func (h DDHandler) attrSep() string {
	return ","
}

func (h DDHandler) newHandleState(buf *buffer.Buffer, freeBuf bool, sep string) handleState {
	s := handleState{
		h:       h,
		buf:     buf,
		freeBuf: freeBuf,
		sep:     sep,
		prefix:  buffer.New(),
	}
	if h.opts.ReplaceAttr != nil {
		s.groups = groupPool.Get().(*[]string)
		*s.groups = append(*s.groups, h.groups[:h.nOpenGroups]...)
	}
	return s
}

func (s *handleState) free() {
	if s.freeBuf {
		s.buf.Free()
	}
	if gs := s.groups; gs != nil {
		*gs = (*gs)[:0]
		groupPool.Put(gs)
	}
	s.prefix.Free()
}

func (s *handleState) openGroups() {
	for _, n := range s.h.groups[s.h.nOpenGroups:] {
		s.openGroup(n)
	}
}

const hex = "0123456789abcdef"

// Copied from encoding/json/tables.go.
//
// safeSet holds the value true if the ASCII character with the given array
// position can be represented inside a JSON string without any further
// escaping.
//
// All values are true except for the ASCII control characters (0-31), the
// double quote ("), and the backslash character ("\").
var safeSet = [utf8.RuneSelf]bool{
	' ':      true,
	'!':      true,
	'"':      false,
	'#':      true,
	'$':      true,
	'%':      true,
	'&':      true,
	'\'':     true,
	'(':      true,
	')':      true,
	'*':      true,
	'+':      true,
	',':      true,
	'-':      true,
	'.':      true,
	'/':      true,
	'0':      true,
	'1':      true,
	'2':      true,
	'3':      true,
	'4':      true,
	'5':      true,
	'6':      true,
	'7':      true,
	'8':      true,
	'9':      true,
	':':      true,
	';':      true,
	'<':      true,
	'=':      true,
	'>':      true,
	'?':      true,
	'@':      true,
	'A':      true,
	'B':      true,
	'C':      true,
	'D':      true,
	'E':      true,
	'F':      true,
	'G':      true,
	'H':      true,
	'I':      true,
	'J':      true,
	'K':      true,
	'L':      true,
	'M':      true,
	'N':      true,
	'O':      true,
	'P':      true,
	'Q':      true,
	'R':      true,
	'S':      true,
	'T':      true,
	'U':      true,
	'V':      true,
	'W':      true,
	'X':      true,
	'Y':      true,
	'Z':      true,
	'[':      true,
	'\\':     false,
	']':      true,
	'^':      true,
	'_':      true,
	'`':      true,
	'a':      true,
	'b':      true,
	'c':      true,
	'd':      true,
	'e':      true,
	'f':      true,
	'g':      true,
	'h':      true,
	'i':      true,
	'j':      true,
	'k':      true,
	'l':      true,
	'm':      true,
	'n':      true,
	'o':      true,
	'p':      true,
	'q':      true,
	'r':      true,
	's':      true,
	't':      true,
	'u':      true,
	'v':      true,
	'w':      true,
	'x':      true,
	'y':      true,
	'z':      true,
	'{':      true,
	'|':      true,
	'}':      true,
	'~':      true,
	'\u007f': true,
}

// handleState holds state for a single call to commonHandler.handle.
// The initial value of sep determines whether to emit a separator
// before the next key, after which it stays true.
type handleState struct {
	h       DDHandler
	buf     *buffer.Buffer
	freeBuf bool           // should buf be freed?
	sep     string         // separator to write before next key
	prefix  *buffer.Buffer // for text: key prefix
	groups  *[]string      // pool-allocated slice of active groups, for ReplaceAttr
}

var groupPool = sync.Pool{
	New: func() any {
		s := make([]string, 0, 10)
		return &s
	},
}

// Separator for group names and keys.
const keyComponentSep = '.'

// openGroup starts a new group of attributes
// with the given name.
func (s *handleState) openGroup(name string) {
	s.appendKey(name)
	s.buf.WriteByte('{')
	s.sep = ""

	// Collect group names for ReplaceAttr.
	if s.groups != nil {
		*s.groups = append(*s.groups, name)
	}
}

// closeGroup ends the group with the given name.
func (s *handleState) closeGroup(name string) {
	s.buf.WriteByte('}')

	s.sep = ","
	if s.groups != nil {
		*s.groups = (*s.groups)[:len(*s.groups)-1]
	}
}

// appendAttrs appends the slice of Attrs.
// It reports whether something was appended.
func (s *handleState) appendAttrs(as []slog.Attr) bool {
	nonEmpty := false
	for _, a := range as {
		if s.appendAttr(a) {
			nonEmpty = true
		}
	}
	return nonEmpty
}

func castValue(v slog.Value) struct {
	_   [0]func()
	num uint64
	any any
} {
	return *(*struct {
		_   [0]func()
		num uint64
		any any
	})(unsafe.Pointer(&v))
}

// appendAttr appends the slog.Attr's key and value.
// It handles replacement and checking for an empty key.
// It reports whether something was appended.
func (s *handleState) appendAttr(a slog.Attr) bool {
	a.Value = a.Value.Resolve()
	if rep := s.h.opts.ReplaceAttr; rep != nil && a.Value.Kind() != slog.KindGroup {
		var gs []string
		if s.groups != nil {
			gs = *s.groups
		}
		// a.Value is resolved before calling ReplaceAttr, so the user doesn't have to.
		a = rep(gs, a)
		// The ReplaceAttr function may return an unresolved slog.Attr.
		a.Value = a.Value.Resolve()
	}
	// Elide empty Attrs.
	if a.Key == "" {
		if v := castValue(a.Value); v.num == 0 && v.any == nil {
			// If the value is zero, it is empty.
			// If it is nil, it is empty.
			return false
		}
	}
	// Special case: Source.
	if v := a.Value; v.Kind() == slog.KindAny {
		if src, ok := v.Any().(*slog.Source); ok && src != nil && *src != (slog.Source{}) {

			as := make([]slog.Attr, 0, 3)
			if src.Function != "" {
				as = append(as, slog.String("function", src.Function))
			}
			if src.File != "" {
				as = append(as, slog.String("file", src.File))
			}
			if src.Line != 0 {
				as = append(as, slog.Int("line", src.Line))
			}

		}
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		// Output only non-empty groups.
		if len(attrs) > 0 {
			// The group may turn out to be empty even though it has attrs (for
			// example, ReplaceAttr may delete all the attrs).
			// So remember where we are in the buffer, to restore the position
			// later if necessary.
			pos := s.buf.Len()
			// Inline a group with an empty key.
			if a.Key != "" {
				s.openGroup(a.Key)
			}
			if !s.appendAttrs(attrs) {
				s.buf.SetLen(pos)
				return false
			}
			if a.Key != "" {
				s.closeGroup(a.Key)
			}
		}
	} else {
		s.appendKey(a.Key)
		s.appendValue(a.Value)
	}
	return true
}

func (s *handleState) appendError(err error) {
	s.appendString(fmt.Sprintf("!ERROR:%v", err))
}

func (s *handleState) appendKey(key string) {
	s.buf.WriteString(s.sep)
	if s.prefix != nil && len(*s.prefix) > 0 {
		s.appendTwoStrings(string(*s.prefix), key)
	} else {
		s.appendString(key)
	}
	s.buf.WriteByte(':')

	s.sep = ","
}

// appendTwoStrings implements appendString(prefix + key), but faster.
func (s *handleState) appendTwoStrings(x, y string) {
	buf := *s.buf
	buf.WriteByte('"')
	buf = appendEscapedJSONString(buf, x)
	buf = appendEscapedJSONString(buf, y)
	buf.WriteByte('"')
	*s.buf = buf
}

// appendEscapedJSONString escapes s for JSON and appends it to buf.
// It does not surround the string in quotation marks.
//
// Modified from encoding/json/encode.go:encodeState.string,
// with escapeHTML set to false.
func appendEscapedJSONString(buf []byte, s string) []byte {
	char := func(b byte) { buf = append(buf, b) }
	str := func(s string) { buf = append(buf, s...) }

	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if safeSet[b] {
				i++
				continue
			}
			if start < i {
				str(s[start:i])
			}
			char('\\')
			switch b {
			case '\\', '"':
				char(b)
			case '\n':
				char('n')
			case '\r':
				char('r')
			case '\t':
				char('t')
			default:
				// This encodes bytes < 0x20 except for \t, \n and \r.
				str(`u00`)
				char(hex[b>>4])
				char(hex[b&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			if start < i {
				str(s[start:i])
			}
			str(`\ufffd`)
			i += size
			start = i
			continue
		}
		// U+2028 is LINE SEPARATOR.
		// U+2029 is PARAGRAPH SEPARATOR.
		// They are both technically valid characters in JSON strings,
		// but don't work in JSONP, which has to be evaluated as JavaScript,
		// and can lead to security holes there. It is valid JSON to
		// escape them, so we do so unconditionally.
		// See http://timelessrepo.com/json-isnt-a-javascript-subset for discussion.
		if c == '\u2028' || c == '\u2029' {
			if start < i {
				str(s[start:i])
			}
			str(`\u202`)
			char(hex[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		str(s[start:])
	}
	return buf
}

func needsQuoting(s string) bool {
	if len(s) == 0 {
		return true
	}
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			// Quote anything except a backslash that would need quoting in a
			// JSON string, as well as space and '='
			if b != '\\' && (b == ' ' || b == '=' || !safeSet[b]) {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return true
		}
		i += size
	}
	return false
}

func (s *handleState) appendString(str string) {
	s.buf.WriteByte('"')
	*s.buf = appendEscapedJSONString(*s.buf, str)
	s.buf.WriteByte('"')
}

func (s *handleState) appendValue(v slog.Value) {
	defer func() {
		if r := recover(); r != nil {
			// Ensure v is not a nil pointer.
			// If it panics with a nil pointer, the most likely cases are
			// an encoding.TextMarshaler or error fails to guard against nil,
			// in which case "<nil>" seems to be the feasible choice.
			//
			// Adapted from the code in fmt/print.go.
			if v := reflect.ValueOf(castValue(v).any); v.Kind() == reflect.Pointer && v.IsNil() {
				s.appendString("<nil>")
				return
			}

			// Otherwise just print the original panic message.
			s.appendString(fmt.Sprintf("!PANIC: %v", r))
		}
	}()

	if err := appendJSONValue(s, v); err != nil {
		s.appendError(err)
	}
}

func appendJSONValue(s *handleState, v slog.Value) error {
	switch v.Kind() {
	case slog.KindString:
		s.appendString(v.String())
	case slog.KindInt64:
		*s.buf = strconv.AppendInt(*s.buf, v.Int64(), 10)
	case slog.KindUint64:
		*s.buf = strconv.AppendUint(*s.buf, v.Uint64(), 10)
	case slog.KindFloat64:
		// json.Marshal is funny about floats; it doesn't
		// always match strconv.AppendFloat. So just call it.
		// That's expensive, but floats are rare.
		if err := appendJSONMarshal(s.buf, v.Float64()); err != nil {
			return err
		}
	case slog.KindBool:
		*s.buf = strconv.AppendBool(*s.buf, v.Bool())
	case slog.KindDuration:
		// Do what json.Marshal does.
		*s.buf = strconv.AppendInt(*s.buf, int64(v.Duration()), 10)
	case slog.KindTime:
		appendJSONTime(s, v.Time())
	case slog.KindAny:
		a := v.Any()
		_, jm := a.(json.Marshaler)
		if err, ok := a.(error); ok && !jm {
			s.appendString(err.Error())
		} else {
			return appendJSONMarshal(s.buf, a)
		}
	default:
		panic(fmt.Sprintf("bad kind: %s", v.Kind()))
	}
	return nil
}

func appendJSONMarshal(buf *buffer.Buffer, v any) error {
	// Use a json.Encoder to avoid escaping HTML.
	var bb bytes.Buffer
	enc := json.NewEncoder(&bb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	bs := bb.Bytes()
	buf.Write(bs[:len(bs)-1]) // remove final newline
	return nil
}

func appendRFC3339Millis(b []byte, t time.Time) []byte {
	// Format according to time.RFC3339Nano since it is highly optimized,
	// but truncate it to use millisecond resolution.
	// Unfortunately, that format trims trailing 0s, so add 1/10 millisecond
	// to guarantee that there are exactly 4 digits after the period.
	const prefixLen = len("2006-01-02T15:04:05.000")
	n := len(b)
	t = t.Truncate(time.Millisecond).Add(time.Millisecond / 10)
	b = t.AppendFormat(b, time.RFC3339Nano)
	b = append(b[:n+prefixLen], b[n+prefixLen+1:]...) // drop the 4th digit
	return b
}

// Adapted from time.Time.MarshalJSON to avoid allocation.
func appendJSONTime(s *handleState, t time.Time) {
	if y := t.Year(); y < 0 || y >= 10000 {
		// RFC 3339 is clear that years are 4 digits exactly.
		// See golang.org/issue/4556#c15 for more discussion.
		s.appendError(errors.New("time.Time year outside of range [0,9999]"))
	}
	s.buf.WriteByte('"')
	*s.buf = t.AppendFormat(*s.buf, time.RFC3339Nano)
	s.buf.WriteByte('"')
}
