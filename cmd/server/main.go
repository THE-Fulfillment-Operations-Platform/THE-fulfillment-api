// Command server is the entrypoint for the THE Fulfillment Operations API.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"log"
	"log/slog"
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
	"the-fulfillment/backend/internal/tracking24h"
)

func main() {
	cfg := config.Load()
	// JSON logs in production so a log collector (or plain grep on structured
	// fields) can filter by status/path/request id. slog.SetDefault also routes
	// the classic log.Printf callers through the same handler. Dev keeps the
	// human-readable default.
	if cfg.IsProduction() {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}
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

	// Shipment tracking provider. Left nil when unconfigured, which keeps
	// tracking a manual field exactly as before.
	trackOpts := services.TrackingOptions{Tag: cfg.Track24hTag, Resolve: cfg.Track24hResolve}
	if cfg.Track24hEnabled {
		trackOpts.Client = tracking24h.New(cfg.Track24hBaseURL, cfg.Track24hEmail, cfg.Track24hPassword)
		log.Printf("tracking: 24hTrack integration on (tag=%s, interval=%s, batch=%d, resolve=%v)",
			cfg.Track24hTag, cfg.Track24hSyncInterval, cfg.Track24hBatchSize, cfg.Track24hResolve)
	}
	// Mockup thumbnail cache for the QC station. Its signing key is derived from
	// the JWT secret rather than configured separately: there is no second secret
	// to deploy or rotate, and a JWT-secret rotation invalidates outstanding
	// thumbnail URLs too — which is the correct behaviour, since those URLs are
	// bearer credentials of their own.
	thumbKey := hmac.New(sha256.New, []byte(cfg.JWTSecret))
	thumbKey.Write([]byte("ffm-thumb-url-v1"))
	thumbOpts := services.ThumbOptions{
		Dir:    cfg.ThumbCacheDir,
		MaxPx:  cfg.ThumbMaxPx,
		URLTTL: cfg.ThumbURLTTL,
		Secret: thumbKey.Sum(nil),
	}

	svc := services.New(repo, jwtManager, carrier, trackOpts, thumbOpts)
	h := handlers.New(svc)
	router := routes.New(cfg, h, jwtManager)

	// Background maintenance: periodically hard-delete rows soft-deleted longer
	// ago than the retention window, so GORM soft-deletes don't pile up forever.
	purgeCtx, stopPurge := context.WithCancel(context.Background())
	defer stopPurge()

	// Keep the connection pool hot. The database is remote, so opening a connection
	// costs a ~1s TCP+TLS+auth handshake, and the pool only ever opens them lazily —
	// on a request that is already waiting. Pinging on a timer pays that in the
	// background instead of charging it to whoever clicks first after a quiet spell.
	if cfg.DBKeepAliveInterval > 0 {
		go database.KeepWarm(purgeCtx, db, cfg.DBMaxIdleConns, cfg.DBKeepAliveInterval)
	}

	if cfg.PurgeEnabled {
		maintenance.NewPurgeScheduler(repo.Admin, cfg.PurgeRetentionDays, cfg.PurgeInterval).Start(purgeCtx)
		log.Printf("maintenance: purge scheduler on (retention=%dd, interval=%s)", cfg.PurgeRetentionDays, cfg.PurgeInterval)
	}

	// Shipment tracking sync. Self-disabling when no provider is configured, so
	// there is no second flag to keep in step with the client above.
	maintenance.NewTrackingScheduler(svc.TrackingSync, cfg.Track24hSyncInterval, cfg.Track24hBatchSize).Start(purgeCtx)

	// Evict mockup thumbnails nobody has looked at in a month, so the cache
	// tracks what is actually in production rather than growing forever.
	if svc.Thumb.Enabled() {
		svc.Thumb.StartSweeper(purgeCtx, cfg.ThumbSweepInterval)
		log.Printf("thumbnails: mockup cache on (dir=%s, max=%dpx)", cfg.ThumbCacheDir, cfg.ThumbMaxPx)
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
		// Generous on purpose: Read must fit an Excel import upload on a slow
		// connection, Write must fit streaming a multi-hundred-MB design zip.
		// They exist so a stalled/malicious client can't hold a connection (and
		// its pooled DB slot upstream) forever — not to police normal requests.
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 15 * time.Minute,
		IdleTimeout:  90 * time.Second,
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
	// Audit entries are written by a background worker; flush what is still queued
	// before the process goes away.
	svc.Audit.Drain(ctx)
	log.Println("server stopped")
}
