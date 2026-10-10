package storage

import (
	"testing"

	"github.com/halfking/goacos/internal/config"
)

func TestEffectiveInstanceHealthDeadlineAndConfiguration(t *testing.T) {
	const now int64 = 100000
	for _, tc := range []struct {
		name      string
		cfg       *config.Config
		heartbeat int64
		ephemeral bool
		storedUp  bool
		want      bool
	}{
		{"just-before-deadline", &config.Config{HeartbeatTimeoutMs: 15000}, now - 14999, true, true, true},
		{"at-deadline", &config.Config{HeartbeatTimeoutMs: 15000}, now - 15000, true, true, true},
		{"past-deadline", &config.Config{HeartbeatTimeoutMs: 15000}, now - 15001, true, true, false},
		{"future-heartbeat", &config.Config{HeartbeatTimeoutMs: 15000}, now + 1, true, true, false},
		{"zero-heartbeat", &config.Config{HeartbeatTimeoutMs: 15000}, 0, true, true, false},
		{"missing-configuration", nil, now, true, true, false},
		{"zero-timeout", &config.Config{}, now, true, true, false},
		{"negative-timeout", &config.Config{HeartbeatTimeoutMs: -1}, now, true, true, false},
		{"explicit-unhealthy", &config.Config{HeartbeatTimeoutMs: 15000}, now, true, false, false},
		{"persistent-healthy", nil, 0, false, true, true},
		{"persistent-unhealthy", nil, now, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Store{cfg: tc.cfg}
			i := &Instance{Healthy: tc.storedUp, Ephemeral: tc.ephemeral, LastHeartbeatMS: tc.heartbeat}
			if got := s.effectiveInstanceHealth(i, now); got != tc.want {
				t.Errorf("effective health=%t, want %t", got, tc.want)
			}
			if i.Healthy != tc.storedUp || i.LastHeartbeatMS != tc.heartbeat {
				t.Error("health derivation must not mutate the supplied stored snapshot")
			}
		})
	}
}
