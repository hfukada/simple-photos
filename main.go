// Command simple-photos is a small S3-backed photo gallery with Google
// sign-in. Photos expire 30 days after upload.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := openDB(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	store, err := newS3Store(ctx, cfg)
	if err != nil {
		log.Fatalf("s3: %v", err)
	}
	if cfg.S3ManageLifecycle {
		if err := store.EnsureLifecycle(ctx, cfg.S3Prefix); err != nil {
			log.Printf("warning: could not set bucket lifecycle rule (expired photos are still removed by the app): %v", err)
		}
	}
	tmpl, err := loadTemplates()
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	app := &App{cfg: cfg, db: db, store: store, oauth: newOAuthConfig(cfg), tmpl: tmpl}
	go app.runReaper(ctx, 15*time.Minute)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute, // slow uplinks with 10 MB photos
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("listening on %s (%s)", cfg.ListenAddr, cfg.BaseURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
