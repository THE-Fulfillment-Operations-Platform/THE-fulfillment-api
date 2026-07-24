// Command server is the entrypoint for the THE Fulfillment Operations API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Embed the IANA timezone database so business-timezone lookups
	// (DB_TIMEZONE, e.g. Asia/Ho_Chi_Minh — used for "STT trong ngày") always
	// resolve, even on a minimal container image without system zoneinfo.
	_ "time/tzdata"

	"the-fulfillment/backend/internal/auth"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/database"
	"the-fulfillment/backend/internal/handlers"
	"the-fulfillment/backend/internal/maintenance"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/routes"
	"the-fulfillment/backend/internal/seed"
	"the-fulfillment/backend/internal/services"
	"the-fulfillment/backend/internal/shipping"
)

func main() {
	cfg := config.Load()
	log.Printf("%s starting (env=%s)", cfg.AppName, cfg.AppEnv)

	// Refuse to boot on insecure production config (default JWT secret, demo
	// accounts with the default password, …). Dev only gets warnings.
	if err := cfg.Validate(); err != nil {
		log.Fatalf("fatal: %v", err)
	}

	// Database + migrations.
	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("fatal: %v", err)
	}
	if err := database.AutoMigrate(db); err != nil {
		log.Fatalf("fatal: %v", err)
	}
	log.Println("database: auto-migration complete")

	// Seed demo data.
	if cfg.SeedOnStart {
		if err := seed.Run(db, cfg); err != nil {
			log.Printf("warning: seed failed: %v", err)
		}
	}

	// Wire dependencies: repositories -> services -> handlers -> routes.
	jwtManager := auth.NewManager(cfg.JWTSecret, cfg.JWTExpiresIn)
	carrier := shipping.NewNoopCarrier("THE") // MVP: no real carrier API yet
	repo := repositories.New(db)
	svc := services.New(repo, jwtManager, carrier)
	h := handlers.New(svc)
	router := routes.New(cfg, h, jwtManager)

	// Background maintenance: periodically hard-delete rows soft-deleted longer
	// ago than the retention window, so GORM soft-deletes don't pile up forever.
	purgeCtx, stopPurge := context.WithCancel(context.Background())
	defer stopPurge()
	if cfg.PurgeEnabled {
		maintenance.NewPurgeScheduler(repo.Admin, cfg.PurgeRetentionDays, cfg.PurgeInterval).Start(purgeCtx)
		log.Printf("maintenance: purge scheduler on (retention=%dd, interval=%s)", cfg.PurgeRetentionDays, cfg.PurgeInterval)
	}

	// Dev convenience: free the port if a previous run left an orphaned process
	// holding it (a `go run` restart gotcha). No-op in production.
	if !cfg.IsProduction() {
		freePortForDev(cfg.Port)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Run server with graceful shutdown.
	go func() {
		log.Printf("listening on http://localhost:%s (docs: /docs)", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("fatal: server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("forced shutdown: %v", err)
	}
	log.Println("server stopped")
}
