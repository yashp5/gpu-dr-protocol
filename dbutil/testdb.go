package dbutil

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	filemutex "github.com/alexflint/go-filemutex"
	"github.com/grene-labs/grene/logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pingcap/log"

	_ "embed"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

var (
	//go:embed schema_copy.sql
	schema string
)
var helponce sync.Once

// pgxpool.Stat as a JSON-serializable struct. See Stat2Response.
type PoolStatResponse struct {
	AcquireCount            int64 `json:"acquire_count"`
	AcquireDurationMS       int64 `json:"acquire_duration"`
	AcquiredConns           int32 `json:"acquired_conns"`
	CanceledAcquireCount    int64 `json:"canceled_acquire_count"`
	ConstructingConns       int32 `json:"constructing_conns"`
	EmptyAcquireCount       int64 `json:"empty_acquire_count"`
	EmptyAcquireWaitTimeMs  int64 `json:"empty_acquire_wait_time"`
	IdleConns               int32 `json:"idle_conns"`
	MaxConns                int32 `json:"max_conns"`
	MaxIdleDestroyCount     int64 `json:"max_idle_destroy_count"`
	MaxLifetimeDestroyCount int64 `json:"max_lifetime_destroy_count"`
	NewConnsCount           int64 `json:"new_conns_count"`
	TotalConns              int32 `json:"total_conns"`
}

func PoolStatToResponse(s *pgxpool.Stat) PoolStatResponse {
	if s == nil {
		return PoolStatResponse{}
	}
	return PoolStatResponse{
		AcquireCount:            s.AcquireCount(),
		AcquiredConns:           s.AcquiredConns(),
		AcquireDurationMS:       s.AcquireDuration().Milliseconds(),
		CanceledAcquireCount:    s.CanceledAcquireCount(),
		ConstructingConns:       s.ConstructingConns(),
		EmptyAcquireCount:       s.EmptyAcquireCount(),
		EmptyAcquireWaitTimeMs:  s.EmptyAcquireWaitTime().Milliseconds(),
		IdleConns:               s.IdleConns(),
		MaxConns:                s.MaxConns(),
		MaxIdleDestroyCount:     s.MaxIdleDestroyCount(),
		MaxLifetimeDestroyCount: s.MaxLifetimeDestroyCount(),
		NewConnsCount:           s.NewConnsCount(),
		TotalConns:              s.TotalConns(),
	}
}

// DialEmbeddedPool returns a connection pool to an embedded postgres instance.
// - try to connect to postgres with user, db, password all 'postgres' on the default postgres port (5432).
// either way, then CREATE IF NOT EXISTS a database named grene_test_xxxxxxxx where xxxxxxxx is random hex.
// then connect to that database with a connection pool, create the schemas, indices, etc.
// the returned stop function will drop the database and stop the embedded postgres instance if we started it.
//
// the upshot of this is that every go instance gets its own database, but they all share the same postgres BINARY and some processes.
// this should be much less heavyweight than our previous solution, which started a new postgres instance for every test and often ran into port conflicts.
// there are still potential race issues.
func DialEmbeddedPool(ctx context.Context) (pool *pgxpool.Pool, err error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	defer func() {
		if err != nil {
			helponce.Do(func() {
				log.Debug(`HELP: there may be a stray postgres instance running on port 5432. 'killall postgres' should usually clean things up.
WARNING: this is a sledgehammer that will kill all postgres instances on that machine. if you do this in prod I will do something very unpleasant indeed. ~efron.`)
			})
		}
	}()
	dbname := fmt.Sprintf("grene_test_%08x", rand.Uint())
	log := logger.FromCtx(ctx).WithGroup("db").With("name", dbname)

	url := embeddedpostgres.DefaultConfig().GetConnectionURL()

	log.Info("checking for existing postgres instance", "url", url)
	// 1: GET CONN
	// 2: CREATE DB IF NOT EXISTS
	// 3: CONNECT TO DB WITH POOL
	// 4: SET TIMEOUTS
	// 5: CREATE SCHEMA
	// 6: RETURN POOL AND STOP FUNC
	// we time each step for logging purposes.
	var watch struct {
		_stop, start                                                                            time.Time
		functions, bots, createpg, createdb, conn, pool, schema, view, latest_view, wait, index time.Duration
	}
	elapsed := func() time.Duration { d := time.Since(watch._stop); watch._stop = time.Now(); return d }
	watch.start = time.Now()
	watch._stop = watch.start
	// 1: GET CONN: get a connection to the 'postgres' database, creating it if necessary.
	// at the end of this block, conn is a valid connection to the 'postgres' database.
	var conn *pgx.Conn
	{

		// does postgres exist already?
		if conn, err = pgx.Connect(ctx, url); err == nil {
			// yes.
			log.Info("connected to existing postgres instance", "url", url)
			watch.conn = elapsed()

			goto CREATE_DB
		}
		log.Info("could not connect to existing postgres instance, starting embedded", "url", url, "err", err)
		lock := filepath.Join(os.TempDir(), "grene/lock/embedded_postgres.lock")
		_ = os.MkdirAll(filepath.Dir(lock), 0o755)
		mux, err := filemutex.New(lock)
		if err != nil {
			return nil, fmt.Errorf("get filemutex %s: %w", lock, err)
		}
		log.Info("acquiring lock to start embedded postgres", "lockfile", lock)
		switch err := mux.TryLock(); err {
		default:
			return nil, fmt.Errorf("get lock %s: %w", lock, err)
		case filemutex.AlreadyLocked: // someone else is starting postgres. wait for them to finish.
			if stat, err := os.Stat(lock); err == nil && time.Since(stat.ModTime()) > 5*time.Minute {
				log.Warn("file lock is very old: something may have gone wrong", "lockfile", lock, "last_mod", stat.ModTime().Format(time.RFC3339), "age", time.Since(stat.ModTime()).String())
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			postgresready := make(chan struct{})
			go func() {
				err := mux.RLock()
				watch.wait = elapsed()
				if err != nil {
					cancel()
					return
				} // wait for the other guy to finish starting postgres.
				close(postgresready)
			}()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-postgresready:
			}
			defer mux.RUnlock()
			conn, err = pgx.Connect(ctx, url)
			if err != nil {
				return nil, fmt.Errorf("connect to embedded postgres %s: %w", url, err)
			}
		case nil: // we are the first to create the lock file: we get to start postgres.
			db := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig())
			watch.createpg = elapsed()

			if err := db.Start(); err != nil {
				return nil, fmt.Errorf("could not start embedded postgres: %w", err)
			}
			defer mux.Unlock()
			conn, err = pgx.Connect(ctx, url)
			if err != nil {
				return nil, fmt.Errorf("connect to embedded postgres %s: %w", url, err)
			}
		}
	}
CREATE_DB: // 2. Create a DB

	if _, err = conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", dbname)); err != nil {
		return nil, fmt.Errorf("CREATE DATABASE  %s: %w", dbname, err)
	}
	watch.createdb = elapsed()
	log.Info("created database if not exists", "url", url, "dbname", dbname)
	conn.Close(ctx) // we no longer need the non-pooled connection (to the wrong database).

	// 3POOL: connect to the new database with a connection pool.
	poolcfg, err := pgxpool.ParseConfig(embeddedpostgres.DefaultConfig().Database(dbname).GetConnectionURL())
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", url, err)
	}
	// // 4: SET TIMEOUTS: set timeouts on every connection.
	// see https://www.postgresql.org/docs/15/runtime-config-client.html for details on these settings.
	poolcfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		const settings = `

-- settings
	set timezone = 'UTC';
	set statement_timeout = '120s';
	set idle_in_transaction_session_timeout = '120s';
	set lock_timeout = '30s';
`
		_, err := conn.Exec(ctx, settings)
		if err != nil {
			return fmt.Errorf("%w: %s", err, settings)
		}
		return nil
	}
	pool, err = pgxpool.NewWithConfig(ctx, poolcfg)
	if err != nil {
		return nil, fmt.Errorf("connect to %s db %s: %w", url, dbname, err)
	}
	watch.pool = elapsed()
	// 3. CREATE SCHEMA
	// 4: Use the pooled connection to build the schema.
	schemas := []struct {
		name, content string
		ptr           *time.Duration
	}{
		// DO NOT REORDER
		{"schema", schema, &watch.schema},
	}
	for i := range schemas {
		if _, err = pool.Exec(ctx, schemas[i].content); err != nil {
			return nil, fmt.Errorf("create %s: %w", schemas[i].name, err)
		}
		*schemas[i].ptr = elapsed()
	}

	// 5. log timings and return the pool.
	log.Info("connected to postgres", slog.Group("timing", "wait", watch.wait,
		"conn", watch.conn.String(),
		"createdb", watch.createdb.String(),
		"createpg", watch.createpg.String(),
		"index", watch.index.String(),
		"latest_view", watch.latest_view.String(),
		"schema", watch.schema.String(),
		"total", time.Since(watch.start).String(),
		"view", watch.view.String(),
	), "url", url, "dbname", dbname)

	return pool, pool.Ping(ctx)
}

// Use DialPool instead. See it's docs for details.
//
// Deprecated: stop() and log are no longer needed. Use DialPool instead.
func OldEmbeddedPool(ctx context.Context, log *slog.Logger) (pool *pgxpool.Pool, stop func() error, err error) {
	if log == nil {
		log = slog.Default()
	}
	pool, err = DialEmbeddedPool(logger.SaveCtx(ctx, log))
	if err != nil {
		return nil, nil, err
	}
	stop = func() error {
		pool.Close()
		return nil
	}
	return pool, stop, nil
}
