// Command basketd serves the cart sync endpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/usmanvirk/offline-basket/internal/httpapi"
	"github.com/usmanvirk/offline-basket/internal/store/memory"
	"github.com/usmanvirk/offline-basket/internal/store/postgres"
)

func main() {
	var (
		addr      = flag.String("addr", ":8080", "listen address")
		dsn       = flag.String("database-url", os.Getenv("DATABASE_URL"), "Postgres DSN, empty for the in memory store")
		retention = flag.Duration("retention", 30*24*time.Hour, "how long an action id is remembered, which is how long a device may be offline and still replay safely")
		sweepFreq = flag.Duration("sweep-every", time.Hour, "how often expired action ids are deleted")
		insecure  = flag.Bool("insecure-header-auth", false, "take the user id from X-User-Id, for local use only")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if !*insecure {
		// There is no token verification in this repository, so the only auth
		// it has is the one that must never face the internet. Refusing to
		// start is better than starting with a cart anybody can read by
		// setting a header.
		log.Error("no authentication is configured; pass -insecure-header-auth to run locally")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux, pg, err := build(ctx, log, *dsn)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	if pg != nil {
		go sweep(ctx, log, pg, *retention, *sweepFreq)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", *addr, "store", storeName(pg))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	// Drain rather than cut. A sync in flight holds a row lock, and killing it
	// mid transaction means the client retries, which is safe but pointless.
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

func storeName(pg *postgres.Store) string {
	if pg != nil {
		return "postgres"
	}
	return "memory"
}

// build returns the routes, and the Postgres store when there is one, so the
// caller knows whether to start the sweeper. A nil store means the service is
// running in memory.
func build(ctx context.Context, log *slog.Logger, dsn string) (*http.ServeMux, *postgres.Store, error) {
	if dsn == "" {
		log.Warn("no database-url, using the in memory store; carts are lost on restart and not shared between replicas")
		return httpapi.New(memory.New(), httpapi.HeaderAuth, log).Routes(), nil, nil
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return nil, nil, err
	}

	pg := postgres.New(pool)
	if err := pg.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return httpapi.New(pg, httpapi.HeaderAuth, log).Routes(), pg, nil
}

// sweep deletes idempotency records past the retention window. It runs on
// every replica, which is harmless: the delete is a no-op for whichever one
// loses the race.
func sweep(ctx context.Context, log *slog.Logger, pg *postgres.Store, retention, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := pg.SweepApplied(ctx, retention)
			if err != nil {
				log.Error("sweep failed", "err", err)
				continue
			}
			if n > 0 {
				log.Info("swept expired action ids", "rows", n, "retention", retention.String())
			}
		}
	}
}
