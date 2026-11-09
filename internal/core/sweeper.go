package core

import (
	"context"
	"log/slog"
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
				slog.Error("naming sweep failed", "error", err)
				continue
			}
			if unhealthy > 0 || deleted > 0 {
				slog.Info("naming sweep completed", "marked_unhealthy", unhealthy, "deleted", deleted)
			}
			// Persistent instances are never deleted for silence, but a silent
			// one must stop being selected — otherwise a dead endpoint stays
			// healthy forever. Separate log line and counter: the two paths
			// have different meanings (ephemeral died, persistent went quiet).
			stale, err := st.MarkStalePersistentUnhealthy(ctx, cfg.PersistentHeartbeatTimeoutMs)
			if err != nil {
				slog.Error("persistent sweep failed", "error", err)
				continue
			}
			if stale > 0 {
				slog.Info("persistent sweep marked unhealthy", "marked_unhealthy", stale,
					"note", "still present, just out of rotation")
			}
		}
	}()
}
