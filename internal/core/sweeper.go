package core

import (
	"context"
	"log"
	"time"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// StartNamingSweeper runs the ephemeral-instance lifecycle loop: mark stale
// instances unhealthy after HeartbeatTimeoutMs, delete them after
// EphemeralDeleteAfterMs (mirrors the Nacos 15s/30s defaults). The loop is
// idempotent, so several goacos replicas may run it concurrently.
func StartNamingSweeper(ctx context.Context, st *storage.Store, cfg *config.Config) {
	go func() {
		t := time.NewTicker(cfg.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			unhealthy, deleted, err := st.SweepEphemeral(ctx, cfg.HeartbeatTimeoutMs, cfg.EphemeralDeleteAfterMs)
			if err != nil {
				log.Printf("[goacos] naming sweep error: %v", err)
				continue
			}
			if unhealthy > 0 || deleted > 0 {
				log.Printf("[goacos] naming sweep: marked unhealthy=%d deleted=%d", unhealthy, deleted)
			}
		}
	}()
}
