package loglimit

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"text/tabwriter"
)

type (
	// Stats is a snapshot of the number of logs dropped or allowed by a StatHandler.
	// Snats fulfill the slog.LogValuer interface.
	Stats struct {
		// DroppedByParent is the number of logs dropped by the parent handler before we got to them.
		Allowed, Dropped, DroppedByParent struct {
			Debug, Info, Warn, Error uint64 // number of logs dropped or allowed per level
			Unknown                  uint64 // other log levels
		}
	}

	StatsSummary struct {
		Allowed, Dropped, DroppedByParent, Total uint64
	}

	logEnabledResult byte // the result of a log.

	stats struct {
		_                                 noCopy
		allowed, dropped, droppedByParent struct {
			Debug, Info, Warn, Error atomic.Uint64
			Unknown                  atomic.Uint64 // other log levels
		}
	}
)

const (
	droppedByParent logEnabledResult = iota // the log was dropped by the parent handler before we got to it.
	dropped                                 // the log was dropped by us.
	allowed                                 // the log was allowed through by us.
)

var _ slog.LogValuer = (*Stats)(nil)

// shorthand for slog.Uint64
func u64(k string, v uint64) slog.Attr { return slog.Uint64(k, v) }

// Attr converts the stats to a slog.Attr for efficient logging.
func (s *Stats) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Group("allowed",
			u64("debug", s.Allowed.Debug),
			u64("info", s.Allowed.Info),
			u64("warn", s.Allowed.Warn),
			u64("error", s.Allowed.Error),
			u64("unknown", s.Allowed.Unknown),
		),
		slog.Group("dropped",
			u64("debug", s.Dropped.Debug),
			u64("info", s.Dropped.Info),
			u64("warn", s.Dropped.Warn),
			u64("error", s.Dropped.Error),
			u64("unknown", s.Dropped.Unknown),
		),
		slog.Group("dropped_by_parent",
			u64("debug", s.DroppedByParent.Debug),
			u64("info", s.DroppedByParent.Info),
			u64("warn", s.DroppedByParent.Warn),
			u64("error", s.DroppedByParent.Error),
			u64("unknown", s.DroppedByParent.Unknown),
		),
	)
}

// Stats returns a snapshot view of the stats.
func (h *Handler) Stats() *Stats { return h.stats.snapshot() }

func (s *Stats) Add(o *Stats) *Stats {
	sum := Stats{
		Allowed: struct {
			Debug, Info, Warn, Error uint64
			Unknown                  uint64
		}{
			Debug:   s.Allowed.Debug + o.Allowed.Debug,
			Info:    s.Allowed.Info + o.Allowed.Info,
			Warn:    s.Allowed.Warn + o.Allowed.Warn,
			Error:   s.Allowed.Error + o.Allowed.Error,
			Unknown: s.Allowed.Unknown + o.Allowed.Unknown,
		},
		Dropped: struct {
			Debug, Info, Warn, Error uint64
			Unknown                  uint64
		}{
			Debug:   s.Dropped.Debug + o.Dropped.Debug,
			Info:    s.Dropped.Info + o.Dropped.Info,
			Warn:    s.Dropped.Warn + o.Dropped.Warn,
			Error:   s.Dropped.Error + o.Dropped.Error,
			Unknown: s.Dropped.Unknown + o.Dropped.Unknown,
		},
		DroppedByParent: struct {
			Debug, Info, Warn, Error uint64
			Unknown                  uint64
		}{
			Debug:   s.DroppedByParent.Debug + o.DroppedByParent.Debug,
			Info:    s.DroppedByParent.Info + o.DroppedByParent.Info,
			Warn:    s.DroppedByParent.Warn + o.DroppedByParent.Warn,
			Error:   s.DroppedByParent.Error + o.DroppedByParent.Error,
			Unknown: s.DroppedByParent.Unknown + o.DroppedByParent.Unknown,
		},
	}
	return &sum
}

// Summary returns the total number of logs allowed and dropped without breaking it down by level.
func (s *Stats) Summary() StatsSummary {
	allowed := s.Allowed.Debug + s.Allowed.Info + s.Allowed.Warn + s.Allowed.Error + s.Allowed.Unknown
	dropped := s.Dropped.Debug + s.Dropped.Info + s.Dropped.Warn + s.Dropped.Error + s.Dropped.Unknown
	droppedByParent := s.DroppedByParent.Debug + s.DroppedByParent.Info + s.DroppedByParent.Warn + s.DroppedByParent.Error + s.DroppedByParent.Unknown

	return StatsSummary{
		Allowed:         allowed,
		Dropped:         dropped,
		DroppedByParent: droppedByParent,
	}
}

// Write the stats to w in a tab-separated format with the following columns: SRC, DEBUG, INFO, WARN, ERROR, UNKNOWN, TOTAL.
func (s *Stats) WriteTo(w io.Writer) (int64, error) {
	const minwidth, tabwidth, padding, padchar, flags = 0, 0, 1, ' ', 0
	tw := tabwriter.NewWriter(w, minwidth, tabwidth, padding, padchar, flags)
	var n int64
	writeRow := func(src, debug, info, warn, error, unknown, total any) error {
		const format = "%v\t%v\t%v\t%v\t%v\t%v\t%v\n"
		m, err := fmt.Fprintf(tw, format, src, debug, info, warn, error, unknown, total)
		n += int64(m)
		return err
	}

	p, d, a := &s.DroppedByParent, &s.Dropped, &s.Allowed
	// add the values of the columns
	sum := func(col *struct{ Debug, Info, Warn, Error, Unknown uint64 }) uint64 {
		return col.Debug + col.Info + col.Warn + col.Error + col.Unknown
	}
	if err := errors.Join(
		writeRow("SRC", "DEBUG", "INFO", "WARN", "ERROR", "UNKNOWN", "TOTAL"),
		writeRow("ALLOWED", a.Debug, a.Info, a.Warn, a.Error, a.Unknown, sum(a)),
		writeRow("DROPPED", d.Debug, d.Info, d.Warn, d.Error, d.Unknown, sum(d)),
		writeRow("DROPPED_BY_PARENT", p.Debug, p.Info, p.Warn, p.Error, p.Unknown, sum(p)),
		writeRow("TOTAL", d.Debug+a.Debug+p.Debug, d.Info+a.Info+p.Info, d.Warn+a.Warn+p.Debug, d.Error+a.Error+p.Error, d.Unknown+a.Unknown+p.Unknown, sum(d)+sum(a)+sum(p)),
		tw.Flush(),
	); err != nil {
		return n, err
	}
	return n, nil
}

// String returns the stats in a tab-separated format with the following columns: SRC, DEBUG, INFO, WARN, ERROR, UNKNOWN, TOTAL.
func (s *Stats) String() string {
	var buf strings.Builder
	s.WriteTo(&buf)
	return buf.String()
}

// Stats are zero if all the counters are zero.
func (s *Stats) IsZero() bool {
	return s.DroppedByParent.Debug == 0 && s.DroppedByParent.Info == 0 && s.DroppedByParent.Warn == 0 && s.DroppedByParent.Error == 0 && s.DroppedByParent.Unknown == 0 &&
		s.Dropped.Debug == 0 && s.Dropped.Info == 0 && s.Dropped.Warn == 0 && s.Dropped.Error == 0 && s.Dropped.Unknown == 0 &&
		s.Allowed.Debug == 0 && s.Allowed.Info == 0 && s.Allowed.Warn == 0 && s.Allowed.Error == 0 && s.Allowed.Unknown == 0
}

// snapshot returns a snapshot view of the stats.
// it does not necessarially return the exact values of the stats at any given point in time,
// and may already be outdated by the time it is returned.
func (s *stats) snapshot() *Stats {
	return &Stats{
		DroppedByParent: struct {
			Debug, Info, Warn, Error uint64
			Unknown                  uint64
		}{
			Debug: s.droppedByParent.Debug.Load(),
			Info:  s.droppedByParent.Info.Load(),
			Warn:  s.droppedByParent.Warn.Load(),
			Error: s.droppedByParent.Error.Load(),
		},
		Dropped: struct {
			Debug, Info, Warn, Error uint64
			Unknown                  uint64
		}{
			Debug:   s.dropped.Debug.Load(),
			Info:    s.dropped.Info.Load(),
			Warn:    s.dropped.Warn.Load(),
			Error:   s.dropped.Error.Load(),
			Unknown: s.dropped.Unknown.Load(),
		},
		Allowed: struct {
			Debug, Info, Warn, Error uint64
			Unknown                  uint64
		}{
			Debug:   s.allowed.Debug.Load(),
			Info:    s.allowed.Info.Load(),
			Warn:    s.allowed.Warn.Load(),
			Error:   s.allowed.Error.Load(),
			Unknown: s.allowed.Unknown.Load(),
		},
	}
}

// update the stats with the result of a log.
func (s *stats) update(level slog.Level, k logEnabledResult) {
	switch k {
	case droppedByParent:
		switch level {
		case slog.LevelDebug:
			s.droppedByParent.Debug.Add(1)
		case slog.LevelInfo:
			s.droppedByParent.Info.Add(1)
		case slog.LevelWarn:
			s.droppedByParent.Warn.Add(1)
		case slog.LevelError:
			s.droppedByParent.Error.Add(1)
		default:
			s.droppedByParent.Unknown.Add(1)
		}
	case dropped:
		switch level {
		case slog.LevelDebug:
			s.dropped.Debug.Add(1)
		case slog.LevelInfo:
			s.dropped.Info.Add(1)
		case slog.LevelWarn:
			s.dropped.Warn.Add(1)
		case slog.LevelError:
			s.dropped.Error.Add(1)
		default:
			s.dropped.Unknown.Add(1)

		}
	case allowed:
		switch level {
		case slog.LevelDebug:
			s.allowed.Debug.Add(1)
		case slog.LevelInfo:
			s.allowed.Info.Add(1)
		case slog.LevelWarn:
			s.allowed.Warn.Add(1)
		case slog.LevelError:
			s.allowed.Error.Add(1)
		default:
			s.allowed.Unknown.Add(1)
		}
	}
}
