// dblog.go contains the LogDB type, which wraps a database connection and logs operations to a *slog.Logger.
package dbutil

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/grene-labs/grene/enve"
	"github.com/grene-labs/grene/logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LogDB wraps a database, logging certain operations.
type LogDB struct {
	count Count
	inner DBTXandBegin
	log   *slog.Logger // Underlying logger for logging database operations. If nil, uses slog.Default().
	lvl   LogDBLevel   // Lvel defines the log levels for different database operations. These all default to slog.LevelInfo.  Use LevelFromEnv to set it from environment variables.
}

// Count returns a handler for the count of calls by caller. This is a web server among other things.
func (db *LogDB) Count() *Count { return &db.count }

var _ DBTXandBegin = (*LogDB)(nil) // Ensure LogDB implements the DBTX interface.
func (db *LogDB) Ping(ctx context.Context) error {
	c := caller(&db.count, "PING;")
	log := logger.FromCtx(ctx).With("caller", c)

	start := time.Now()
	log.Log(ctx, db.lvl.ExecRequest, "sql: ping")
	err := db.inner.Ping(ctx)
	elapsed := time.Since(start)
	if err != nil {
		log.Log(ctx, db.lvl.Error, "sql: ping error", "err", err, "elapsed", elapsed)
		return err
	}
	log.Log(ctx, db.lvl.ExecOK, "sql: ping ok", "elapsed", elapsed)
	return nil
}

// NewLogDB creates a new LogDB instance with the provided database connection, logger, and log levels.
// If db is nil, it panics. If log is nil, it uses slog.Default(). If level is nil, it uses LevelFromEnv to set default log levels from environment variables.
func NewLogDB(db DBTXandBegin, log *slog.Logger, level *LogDBLevel) *LogDB {
	if db == nil {
		panic("db is nil")
	}
	if log == nil {
		log = slog.Default()
	}

	if level == nil {
		lvl := LevelFromEnv()
		level = &lvl
	}
	return &LogDB{
		count: Count{},
		inner: db,
		log:   log,
		lvl:   *level,
	}
}

func (db *LogDB) Stat() *pgxpool.Stat {
	if s, ok := db.inner.(interface{ Stat() *pgxpool.Stat }); ok {
		return s.Stat()
	}
	return nil
}
func (db *LogDB) Level() LogDBLevel { return db.lvl }

var errUnknownLogLevel = errors.New("unknown log level: valid levels are debug, info, warn, error")

func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug", "d":
		return slog.LevelDebug, nil
	case "info", "i":
		return slog.LevelInfo, nil
	case "warn", "warning", "w":
		return slog.LevelWarn, nil
	case "error", "err", "e":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, errUnknownLogLevel
	}
}

// LevelFromEnv returns a LogDBLevel with log levels set from environment variables.
func LevelFromEnv() LogDBLevel {
	// Default log levels for database operations.
	return LogDBLevel{
		ExecRequest:  enve.Or(parseLogLevel, "DB_LOG_EXEC_REQUEST", slog.LevelDebug),
		QueryRequest: enve.Or(parseLogLevel, "DB_LOG_QUERY_REQUEST", slog.LevelDebug),
		ExecOK:       enve.Or(parseLogLevel, "DB_LOG_EXEC_OK", slog.LevelDebug),
		QueryOK:      enve.Or(parseLogLevel, "DB_LOG_QUERY_OK", slog.LevelDebug),
		Error:        enve.Or(parseLogLevel, "DB_LOG_ERROR", slog.LevelWarn),
	}
}

// Configuration for LogDB logging levels.
// Build it from environment variables using LevelFromEnv.
type LogDBLevel struct {
	ExecRequest, QueryRequest slog.Level
	ExecOK, QueryOK           slog.Level
	Error                     slog.Level
}

// as sqlc.DBTX, but also has
// Begin(context.Context) (pgx.Tx, error)
// BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
// Ping(context.Context) error
// Implemented by *pgxpool.Pool, *pgxpool.Conn, *pgx.Conn, *pgx.Tx, *dbutil.LogDB, etc.
type DBTXandBegin interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	Begin(context.Context) (pgx.Tx, error)
	Ping(context.Context) error
}

func (db *LogDB) Inner() DBTXandBegin { return db.inner }

// Exec wraps the underlying database Exec method, logging the access, arguments, elapsed time, and any errors.
func (db *LogDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c := caller(&db.count, sql)
	log := logger.FromCtx(ctx).With("caller", c)
	start := time.Now()
	log.Log(ctx, db.lvl.ExecRequest, "sql: exec")
	tag, err := db.inner.Exec(ctx, sql, args...)
	elapsed := time.Since(start)
	if err != nil {
		log.Log(ctx, db.lvl.Error, "sql: exec error", "err", err, "elapsed", elapsed.String())
		return tag, err
	}
	log.Log(ctx, db.lvl.ExecOK, "sql: exec ok", "elapsed", elapsed.String(), "rows", tag.RowsAffected())
	return tag, nil
}

// Query wraps the underlying database Query method, logging the access, arguments, elapsed time, and any errors.
func (db *LogDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c := caller(&db.count, sql)
	log := logger.FromCtx(ctx).With("caller", c)

	start := time.Now()
	log.Log(ctx, db.lvl.QueryRequest, "sql: query")
	rows, err := db.inner.Query(ctx, sql, args...)
	elapsed := time.Since(start)
	if err != nil {
		log.Log(ctx, db.lvl.Error, "sql: query error", "err", err, "elapsed", elapsed)
		return nil, err
	}
	log.Log(ctx, db.lvl.QueryOK, "sql: query ok", "elapsed", elapsed)
	return rows, nil
}

// QueryRow wraps the underlying database QueryRow method, logging the access, arguments, elapsed time, and any errors.
func (db *LogDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c := caller(&db.count, sql)
	log := logger.FromCtx(ctx).With("caller", c)

	start := time.Now()
	log.Log(ctx, db.lvl.QueryRequest, "sql: query row")
	row := db.inner.QueryRow(ctx, sql, args...)
	elapsed := time.Since(start)
	log.Log(ctx, db.lvl.QueryOK, "sql: query row: ok or no rows", "elapsed", elapsed)
	return row
}
