// Command dieyet serves the nutrition log: a single binary with the frontend
// embedded and a SQLite file next to it.
//
// Environment:
//
//	DIEYET_ADDR             listen address (default 127.0.0.1:8080)
//	DIEYET_DATABASE_URL     PostgreSQL URL, e.g. postgres://dieyet:secret@db.lan:5432/workout?sslmode=disable
//	DIEYET_DATABASE_URL_FILE  path of a file holding that URL instead (Docker secrets)
//	DIEYET_INSECURE_COOKIE  set to 1 for plain-http local development only
//	DIEYET_OFF              set to 0 to disable Open Food Facts lookups
//	DIEYET_PRUNE_IDLE_DAYS  delete accounts with no food logs after N days (default 0 = never;
//	                        keep it off when the users table is shared with another app)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jPurin-gg/dieyet/internal/off"
	"github.com/jPurin-gg/dieyet/internal/server"
	"github.com/jPurin-gg/dieyet/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	addr := envOr("DIEYET_ADDR", "127.0.0.1:8080")
	dsn := os.Getenv("DIEYET_DATABASE_URL")
	if path := os.Getenv("DIEYET_DATABASE_URL_FILE"); dsn == "" && path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("DIEYET_DATABASE_URL_FILE: %w", err)
		}
		dsn = strings.TrimSpace(string(b))
	}
	if dsn == "" {
		return errors.New("DIEYET_DATABASE_URL (or DIEYET_DATABASE_URL_FILE) is required, e.g. postgres://dieyet:secret@db.lan:5432/workout?sslmode=disable")
	}
	dbURL, err := url.Parse(dsn)
	if err != nil {
		return errors.New("DIEYET_DATABASE_URL must be a postgres:// URL")
	}
	secureCookie := os.Getenv("DIEYET_INSECURE_COOKIE") == ""
	offEnabled := os.Getenv("DIEYET_OFF") != "0"
	pruneDays, err := strconv.Atoi(envOr("DIEYET_PRUNE_IDLE_DAYS", "0"))
	if err != nil || pruneDays < 0 {
		return errors.New("DIEYET_PRUNE_IDLE_DAYS must be a non-negative integer")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openWithRetry(ctx, dsn, logger)
	if err != nil {
		return err
	}
	defer st.Close()

	var offClient *off.Client
	if offEnabled {
		offClient = off.New("")
	}
	srv := server.New(st, server.Config{SecureCookie: secureCookie, OFF: offClient, Logger: logger})

	hs := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}

	go maintenance(ctx, st, logger, pruneDays)

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr, "db", dbURL.Host+dbURL.Path, "secure_cookie", secureCookie, "open_food_facts", offEnabled, "prune_idle_days", pruneDays)
		errc <- hs.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return hs.Shutdown(shutdownCtx)
	}
}

// openWithRetry keeps trying to reach the database for a minute, so a reboot
// where PostgreSQL comes up after this process does not end in a crash loop.
// Anything other than a connection failure (e.g. a migration error) is
// returned at once.
func openWithRetry(ctx context.Context, dsn string, logger *slog.Logger) (*store.Store, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, err := store.Open(ctx, dsn)
		if err == nil || !errors.Is(err, store.ErrConnect) || time.Now().After(deadline) {
			return st, err
		}
		logger.Warn("database not reachable yet, retrying", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// maintenance drops expired sessions hourly and, when enabled, once a day
// deletes accounts that were created but never used (typos on the login
// screen create those).
func maintenance(ctx context.Context, st *store.Store, logger *slog.Logger, pruneDays int) {
	hourly := time.NewTicker(time.Hour)
	daily := time.NewTicker(24 * time.Hour)
	defer hourly.Stop()
	defer daily.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hourly.C:
			if n, err := st.DeleteExpiredSessions(ctx, time.Now()); err != nil {
				logger.Warn("delete expired sessions", "err", err)
			} else if n > 0 {
				logger.Info("deleted expired sessions", "n", n)
			}
		case <-daily.C:
			if pruneDays == 0 {
				continue
			}
			if n, err := st.PruneIdleUsers(ctx, time.Now().Add(-time.Duration(pruneDays)*24*time.Hour)); err != nil {
				logger.Warn("prune idle users", "err", err)
			} else if n > 0 {
				logger.Info("pruned idle users", "n", n)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
