package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/grene-labs/grene/logger"

	"github.com/grene-labs/grene/dbutil"
	"github.com/grene-labs/grene/generated/sqlc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/grene-labs/grene/enve"
)

// Run protocol until the context is canceled.
// That is, Start() protocol, wait for a shutdown signal, then gracefully shut down the server.
func Run(ctx context.Context, logger *slog.Logger) error {
	server, cleanup, err := Start(ctx, logger)
	if err != nil {
		return fmt.Errorf("start protocol: %w", err)
	}
	<-ctx.Done()
	defer func() {
		for i := range cleanup {
			if err := cleanup[i](); err != nil {
				logger.Error("cleanup err", "err", err)
			}
		}
	}()
	shutdownTime := enve.DurationOr("PROTOCOL_HTTP_SHUTDOWN_TIMEOUT", 5*time.Second)
	logger.Warn(fmt.Sprintf("got shutdown signal: shutting down in %s", shutdownTime))
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTime)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("dirty shutdown " + err.Error())
	} else {
		logger.Info("clean shutdown")
	}
	return nil
}

// start the Grene protocol, but don't wait for it to finish: use Run() for that.
// this includes:
//   - initializing the database connection (including embedded postgres for local development)
//   - initializing the HTTP response cache
//   - starting the HTTP server
//   - this function only returns on network error or context cancellation.
func Start(ctx context.Context, logger *slog.Logger) (server *http.Server, cleanup []func() error, err error) {
	var responseCache *cache.JSONResponse
	logger.Info("init cache")
	{ // init cache
		var (
			cleanup = enve.DurationOr("DORA_CACHE_CLEANUP_INTERVAL", 10*time.Minute)
			cutoff  = enve.DurationOr("DORA_CACHE_CLEANUP_CUTOFF_AGE", 15*time.Minute)
			target  = enve.IntOr("DORA_CACHE_TARGET_SIZE", 100*1024*1024)
		)
		responseCache, _ = cache.NewJSONResponse(
			cleanup,
			cutoff,
			target,
		)
		logger.Info("cache build", slog.Group("cache", "cleanup", cleanup, "cutoff", cutoff, "target", target))

	}
	var db *dbutil.LogDB
	logger.Info("init DB")
	{ // init DB: either embedded (default) or via google cloud...
		ctx, cancel := context.WithTimeout(ctx, enve.DurationOr("DORA_DB_INIT_TIMEOUT", 30*time.Second))
		defer cancel()
		var pool *pgxpool.Pool
		if enve.BoolOr("DORA_USE_EMBEDDED_DB", true) {
			var err error
			pool, err = dbutil.DialEmbeddedPool(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("init DB: could not create embedded postgres pool: %w", err)
			}
		} else {
			logger.Info("init DB: using external postgres via environment")
			var err error
			pool, err = dbFromEnv(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("init DB: could not connect to external postgres: %w", err)
			}

			cleanup = append(cleanup, func() error { pool.Close(); return nil })
		}
		db = dbutil.NewLogDB(pool, logger, nil)

		if enve.BoolOr("DORA_USE_EMBEDDED_DB", true) || enve.BoolOr("DORA_RUN_E2E_TESTS", false) {
			if err := testutils.InitAdminAndUsdAsset(ctx, sqlc.New(db), logger); err != nil {
				return nil, nil, fmt.Errorf("init DB: could not initialize admin user and USD asset: %w", err)
			}
		}
	}

	logger.Info("init server")
	// init server
	{
		addr := enve.StringOr("DORA_HTTP_SERVE_ADDR", ":8084")
		readtimeout := enve.DurationOr("DORA_HTTP_READ_TIMEOUT", 10*time.Second)
		writetimeout := enve.DurationOr("DORA_HTTP_WRITE_TIMEOUT", 360*time.Second)
		idletimeout := enve.DurationOr("DORA_HTTP_IDLE_TIMEOUT", 180*time.Second)
		failsafeTimeout := enve.DurationOr("DORA_HTTP_FAILSAFE_TIMEOUT", 15*time.Minute)
		server = &http.Server{
			Addr: addr,
			// add the connection to the context for later use (logging, tracking, etc)
			// and also set a failsafe timeout to prevent slowloris-style attacks (or just buggy clients or servers)
			ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
				ctx, cancel := context.WithTimeout(ctx, failsafeTimeout)
				ctx = conndbg.SaveConnCtx(ctx, conn)
				go func() { // when the context is done, close the connection, regardless of what state it's in.
					<-ctx.Done()
					cancel()
					_ = conn.Close()
				}()
				return ctx
			},
			ConnState:    conndbg.TrackState,
			Handler:      buildHTTPRouter(logger, db, responseCache),
			ReadTimeout:  readtimeout,
			WriteTimeout: writetimeout,
			IdleTimeout:  idletimeout,
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	cleanup = append(cleanup, func() error { cancel(); return nil })
	go func() {
		logger.Info("serving HTTP at " + server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "err", err)
		}
	}()
	time.Sleep(15 * time.Millisecond)

	logger.Info("DORA server started")

	return server, cleanup, nil
}

func dbFromEnv(ctx context.Context) (*pgxpool.Pool, error) {
	connstr := enve.StringOr("DATABASE_URL", "") // set by fly.io
	if connstr == "" {
		return nil, errors.New("DATABASE_URL not set")
	}

	cfg, err := pgxpool.ParseConfig(connstr)
	if err != nil {
		return nil, fmt.Errorf("could not parse DATABASE_URL: %w", err)
	}
	cfg.MaxConnIdleTime = 3 * time.Minute
	cfg.MaxConnLifetime = 20 * time.Minute
	cfg.MaxConnLifetimeJitter = time.Minute

	cfg.AfterConnect = afterConnect
	cfg.HealthCheckPeriod = 1 * time.Minute

	return pgxpool.New(ctx, connstr)
}

func afterConnect(ctx context.Context, c *pgx.Conn) error {
	for _, s := range []string{
		"SET TIME ZONE 'UTC';",
		"SET statement_timeout = '60s';",
		"SET idle_in_transaction_session_timeout = '2min';",
		"SET lock_timeout = '15s';",
	} {
		var attrs []slog.Attr
		var anyErr bool
		if _, err := c.Exec(ctx, s); err != nil {
			attrs = append(attrs, slog.String(s, err.Error()))
			anyErr = true
		} else {
			attrs = append(attrs, slog.String(s, "ok"))
		}
		if anyErr {
			logger.FromCtx(ctx).LogAttrs(ctx, slog.LevelWarn, "db: fromEnv: not all settings applied", attrs...)
		} else {
			logger.FromCtx(ctx).LogAttrs(ctx, slog.LevelInfo, "db: fromEnv: all settings applied", attrs...)
		}
	}
	pingStart := time.Now()
	if err := c.Ping(ctx); err != nil {
		return fmt.Errorf("could not ping database: %w", err)
	}
	pingDuration := time.Since(pingStart)
	if pingDuration > 300*time.Millisecond {
		return fmt.Errorf("ping took too long: %s", pingDuration)
	}
	return nil
}
