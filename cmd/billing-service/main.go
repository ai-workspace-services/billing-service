package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"

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

	db, err := otelsql.Open("pgx", cfg.DatabaseURL)
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

	logger := slog.Default()

	// Initialize and start FinOps Syncer (Cloud Billing)
	finopsSyncer := service.NewFinOpsSyncer(repo, logger, cfg)
	go finopsSyncer.Start(ctx)

	suspendSyncer := service.NewSuspendSyncer(repo, cfg.ArrearsSuspendThreshold, cfg.ArrearsSweepInterval, logger)
	go suspendSyncer.Start(ctx)

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: otelhttp.NewHandler(httpapi.New(svc, cfg.InternalServiceToken).Routes(), "billing.request"),
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
