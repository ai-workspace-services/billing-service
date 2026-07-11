package main

import (
	"context"
	"database/sql"
	"log"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"

	"billing-service/internal/config"
	"billing-service/internal/exporter"
	"billing-service/internal/httpapi"
	"billing-service/internal/repository"
	"billing-service/internal/service"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	repo := repository.NewPostgres(db)
	svc := service.New(
		cfg,
		exporter.NewClient(cfg.InternalServiceToken),
		repo,
	)
	svc.Start(ctx)

	// Initialize and start FinOps Syncer (Cloud Billing)
	logger := slog.Default()
	finopsSyncer := service.NewFinOpsSyncer(repo, logger)
	go finopsSyncer.Start(ctx)

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: httpapi.New(svc).Routes(),
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
