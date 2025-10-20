package dbutil

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/grene-labs/grene/logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ pgx.Tx = (*logtx)(nil) // ensure pgx.Tx implements the pgx.Tx interface

// get a logtx via (*LogDB).Begin()
type logtx struct {
	inner   pgx.Tx // underlying transaction
	count   *Count
	args    [][]any
	depth   int
	log     *slog.Logger
	lvl     LogDBLevel
	queries []string
	spans   []uuid.UUID // one or more
	start   time.Time
}

// Begin a sub-transaction.
func (tx *logtx) Begin(ctx context.Context) (pgx.Tx, error) {
	child, err := tx.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	spans := append(tx.spans, id)
	start := time.Now()
	return &logtx{
		count: tx.count,
		inner: child,
		log:   tx.log.With("tx_depth", tx.depth+1, "tx_spans", spans, "tx_start", start),
		lvl:   tx.lvl,
		spans: spans,
		start: start,
	}, err
}

func (db *LogDB) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	db.log.Debug("begin tx with options", "tx_options", txOptions)
	child, err := db.inner.BeginTx(ctx, txOptions)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}

	// start a new sublogger for this nested transaction so we can keep track.
	spans := []uuid.UUID{id}
	start := time.Now()
	return &logtx{
		count: &db.count,
		inner: child,
		spans: spans,
		depth: 0,
		start: start,
		lvl:   db.lvl,
		log:   db.log.With("tx_spans", spans, "tx_depth", 0, "tx_start", start),
	}, err
}

// Begin a transaction, as though calling BeginTx(ctx, pgx.TxOptions{}).
func (db *LogDB) Begin(ctx context.Context) (pgx.Tx, error) { return db.BeginTx(ctx, pgx.TxOptions{}) }

// Get the underlying database connection handle.
func (tx *logtx) Conn() *pgx.Conn { return tx.inner.Conn() }

// CopyFrom the specified table, delegating to the inner transaction. This will log attempts.
func (tx *logtx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	log := tx.log.With("columns", columnNames).Log
	log(ctx, tx.lvl.ExecRequest, "copy from: begin", "columns", columnNames)
	got, err := tx.inner.CopyFrom(ctx, tableName, columnNames, rowSrc)
	if err != nil {
		log(ctx, tx.lvl.Error, "copy from: err", "err", err)
		return got, err
	}
	log(ctx, tx.lvl.ExecOK, "copy from: ok")
	return got, nil
}

// Get the LargeObjects interface from the underlying transaction.
func (tx *logtx) LargeObjects() pgx.LargeObjects { return tx.inner.LargeObjects() }

// Prepare a statement, delegating to the underlying transaction.
func (tx *logtx) Prepare(ctx context.Context, name string, sql string) (desc *pgconn.StatementDescription, err error) {
	log := tx.log.With("name", name, "sql", sql).Log
	log(ctx, tx.lvl.ExecRequest, "prepare statement: begin")
	got, err := tx.inner.Prepare(ctx, name, sql)
	if err != nil {
		log(ctx, tx.lvl.Error, "prepare statement error", "err", err)
	} else {
		log(ctx, tx.lvl.ExecOK, "prepare statement")
	}
	return got, err
}

// Rollback using the inner transaction, logging the results. Only one of Commit()
// and Rollback() can succeed.
func (tx *logtx) Rollback(ctx context.Context) error {
	tx.log.Log(ctx, tx.lvl.ExecRequest, "rollback: begin")
	err := tx.inner.Rollback(ctx)

	if err != nil && err != pgx.ErrTxClosed {
		tx.log.Log(ctx, tx.lvl.Error, "rollback: error", "err", err, "queries", tx.queries, "args", tx.args, "tx_elapsed", time.Since(tx.start))
	} else if err != nil {
		tx.log.Log(ctx, tx.lvl.ExecOK, "rollback: already closed", "queries", tx.queries, "args", tx.args, "tx_elapsed", time.Since(tx.start), "err", err)
	} else {
		tx.log.Log(ctx, tx.lvl.ExecOK, "rollback: ok", "queries", tx.queries, "args", tx.args, "tx_elapsed", time.Since(tx.start))
	}
	// regardless of success or failure, stop holding on to all the references so that we don't leak more memory than necessary
	// if someone is holding on to the *logtx too long.
	clear(tx.queries)
	clear(tx.args)
	return err
}

// Send a batch. The caller must log the result, if any.
func (tx *logtx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	tx.log.Log(ctx, tx.lvl.ExecRequest, "send batch: begin")
	return tx.inner.SendBatch(ctx, b)
}

// Exec wraps the underlying database Exec method, logging the access, arguments, elapsed time, and any errors.
func (tx *logtx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c := caller(tx.count, sql)
	log := logger.FromCtx(ctx).With("caller", c)

	start := time.Now()
	log.Log(ctx, tx.lvl.ExecRequest, "sql: exec")
	tag, err := tx.inner.Exec(ctx, sql, args...)
	elapsed := time.Since(start)
	tx.queries = append(tx.queries, sql)
	tx.args = append(tx.args, args)
	if err != nil {
		log.Log(ctx, tx.lvl.Error, "sql: exec error", "err", err, "elapsed", elapsed)
		return tag, err
	}
	log.Log(ctx, tx.lvl.ExecOK, "sql: exec ok", "elapsed", elapsed.String(), "rows", tag.RowsAffected())
	return tag, nil
}

// Attempt to commit the transaction. Only one of Commit() or Rollback() can succeed.
func (tx *logtx) Commit(ctx context.Context) error {
	tx.log.Log(ctx, tx.lvl.ExecRequest, "commit: begin")
	err := tx.inner.Commit(ctx)
	if err != nil {
		tx.log.Log(ctx, tx.lvl.Error, "commit: error", "err", err, "queries", tx.queries, "args", tx.args, "tx_elapsed", time.Since(tx.start))
	} else {
		tx.log.Log(ctx, tx.lvl.ExecOK.Level(), "commit: ok", "queries", tx.queries, "args", tx.args, "tx_elapsed", time.Since(tx.start))
	}
	// regardless of success or failure, stop holding on to all the references so that we don't leak more memory than necessary
	// if someone is holding on to the *logtx too long.

	clear(tx.queries)
	clear(tx.args)
	return err
}

// Query wraps the underlying database Query method, logging the access, arguments, elapsed time, and any errors.
func (tx *logtx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c := caller(tx.count, sql)
	log := logger.FromCtx(ctx).With("caller", c)

	start := time.Now()
	log.Log(ctx, tx.lvl.QueryRequest, "sql: query")
	tx.queries = append(tx.queries, sql)
	tx.args = append(tx.args, args)
	rows, err := tx.inner.Query(ctx, sql, args...)
	elapsed := time.Since(start)
	if err != nil {
		log.Log(ctx, tx.lvl.Error, "sql: query error", "err", err, "elapsed", elapsed)
		return nil, err
	}
	log.Log(ctx, tx.lvl.QueryOK, "sql: query ok", "elapsed", elapsed)
	return rows, nil
}

// QueryRow wraps the underlying database QueryRow method, logging the access, arguments, elapsed time, and any errors.
func (tx *logtx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c := caller(tx.count, sql)
	log := logger.FromCtx(ctx).With("caller", c)

	start := time.Now()
	tx.queries = append(tx.queries, sql)
	tx.args = append(tx.args, args)
	log.Log(ctx, tx.lvl.QueryRequest, "sql: query row")

	row := tx.inner.QueryRow(ctx, sql, args...)
	elapsed := time.Since(start)
	log.Log(ctx, tx.lvl.QueryOK, "sql: query row: ok or no rows", "elapsed", elapsed)
	return row
}
