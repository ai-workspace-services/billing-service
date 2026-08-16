package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"billing-service/internal/config"
	"billing-service/internal/exporter"
	"billing-service/internal/httpapi"
	"billing-service/internal/observability"
	"billing-service/internal/repository"
	"billing-service/internal/service"

	"github.com/XSAM/otelsql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := observability.Configure(ctx, "web-saas-billing")
	if err != nil {
		log.Printf("OTLP tracing disabled: %v", err)
	} else {
		defer func() {
			if err := shutdownTracing(context.Background()); err != nil {
				log.Printf("flush OTLP traces: %v", err)
			}
		}()
	}

	db, err := openDatabase(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	repo := repository.NewPostgres(db)
	svc := service.New(
		cfg,
		exporter.NewClient(cfg.InternalServiceToken),
		repo,
	)
	svc.Start(ctx)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{})).With(
		observability.RuntimeLogAttrs("web-saas-billing")...,
	)
	slog.SetDefault(logger)

	// Initialize and start FinOps Syncer (Cloud Billing)
	finopsSyncer := service.NewFinOpsSyncer(repo, logger, cfg)
	go finopsSyncer.Start(ctx)

	suspendSyncer := service.NewSuspendSyncer(repo, cfg.ArrearsSuspendThreshold, cfg.ArrearsSweepInterval, logger)
	go suspendSyncer.Start(ctx)

	server := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: otelhttp.NewHandler(
			observability.RequestLogger(logger, httpapi.New(svc, cfg.InternalServiceToken, db).Routes()),
			"billing.request",
		),
	}

	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()

	log.Printf("billing-service listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// openDatabase validates the one configured primary before the HTTP server is
// exposed. It retries startup races (for example, a local PostgreSQL container
// or a freshly reachable Supabase pooler), but never tries another database
// endpoint. Switching primary is an explicit deployment/Vault change followed
// by a process restart.
func openDatabase(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	var lastErr error
	for attempt := 1; attempt <= cfg.DatabaseStartupRetries; attempt++ {
		db, err := otelsql.Open("pgx", cfg.DatabaseURL)
		if err == nil {
			if cfg.DatabaseMaxOpenConns > 0 {
				db.SetMaxOpenConns(cfg.DatabaseMaxOpenConns)
			}
			if cfg.DatabaseMaxIdleConns >= 0 {
				db.SetMaxIdleConns(cfg.DatabaseMaxIdleConns)
			}
			pingCtx, cancel := context.WithTimeout(ctx, cfg.DatabasePingTimeout)
			err = db.PingContext(pingCtx)
			cancel()
		}
		if err == nil {
			return db, nil
		}
		lastErr = err
		if db != nil {
			_ = db.Close()
		}
		if attempt == cfg.DatabaseStartupRetries {
			break
		}
		timer := time.NewTimer(cfg.DatabaseRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("database primary is not ready after %d attempts: %w", cfg.DatabaseStartupRetries, lastErr)
}
