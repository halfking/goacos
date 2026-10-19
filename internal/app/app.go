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
	"github.com/halfking/goacos/internal/gateway"
	"github.com/halfking/goacos/internal/storage"
)

// RunServer opens the database (auto-create + schema + seed + migrations), then
// serves two surfaces until interrupted: the Nacos-compatible API and, when
// enabled, the gateway data plane.
func RunServer(cfg *config.Config) error {
	if err := cfg.ValidateAuth(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	st, err := storage.Open(openCtx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Printf("[goacos] attached to %s %s db=%s", st.Dialect(), cfg.MySQLAddr(), cfg.MySQLDB)

	hub, err := core.NewConfigHub(openCtx, st)
	if err != nil {
		return err
	}
	auth := core.NewAuthService(cfg, st)

	// The gateway is built before the API server so a routing or certificate
	// problem is reported at startup rather than discovered by the first
	// visitor. It is a separate surface on separate ports: the Nacos API keeps
	// answering on its own port for stock clients either way.
	var gw *gateway.Gateway
	if cfg.GatewayEnabled {
		gw, err = gateway.New(openCtx, cfg, st)
		if err != nil {
			return err
		}
	}

	srv := &api.Server{Cfg: cfg, ST: st, Hub: hub, Auth: auth, Started: time.Now()}
	if gw != nil {
		srv.Gateway = gw
	}
	core.StartNamingSweeper(ctx, st, cfg)

	if gw != nil {
		go func() {
			if err := gw.Run(ctx); err != nil {
				log.Printf("[gateway] stopped: %v", err)
			}
		}()
	}

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
