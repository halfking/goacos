// Package app boots the goacos HTTP server.
package app

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/halfking/goacos/internal/api"
	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

// RunServer opens MySQL (auto-create database + schema + seed), then serves
// the Nacos-compatible HTTP API until interrupted.
func RunServer(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	st, err := storage.Open(openCtx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Printf("[goacos] attached to mysql %s db=%s", cfg.MySQLAddr(), cfg.MySQLDB)

	hub, err := core.NewConfigHub(openCtx, st)
	if err != nil {
		return err
	}
	auth := core.NewAuthService(cfg, st)
	srv := &api.Server{Cfg: cfg, ST: st, Hub: hub, Auth: auth, Started: time.Now()}
	core.StartNamingSweeper(ctx, st, cfg)

	httpSrv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	log.Printf("[goacos] listening on %s (auth=%v, console=/nacos/index.html)", cfg.Addr(), cfg.AuthEnabled)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Printf("[goacos] shutdown complete")
	return nil
}
