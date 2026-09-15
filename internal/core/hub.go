// Package core holds in-process services shared by the API handlers.
package core

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/halfking/goacos/internal/storage"
)

// ConfigKey identifies one config entry.
type ConfigKey struct {
	Tenant string
	Group  string
	DataId string
}

// ConfigHub keeps an in-memory MD5 index of all configs and implements the
// long-polling wait used by the Nacos v1/v2 listener endpoints. It is the
// reason goacos stays efficient on modest hardware: listeners park on a
// channel instead of hammering MySQL.
type ConfigHub struct {
	mu       sync.RWMutex
	md5s     map[ConfigKey]string
	watchers map[ConfigKey]map[chan struct{}]struct{}
}

// NewConfigHub preloads the MD5 index from MySQL.
func NewConfigHub(ctx context.Context, st *storage.Store) (*ConfigHub, error) {
	h := &ConfigHub{
		md5s:     map[ConfigKey]string{},
		watchers: map[ConfigKey]map[chan struct{}]struct{}{},
	}
	rows, err := st.DB.QueryContext(ctx, "SELECT data_id, group_id, tenant_id, COALESCE(md5,'') FROM config_info")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k ConfigKey
		var md5v string
		if err := rows.Scan(&k.DataId, &k.Group, &k.Tenant, &md5v); err != nil {
			return nil, err
		}
		h.md5s[k] = md5v
	}
	log.Printf("[goacos] config hub loaded %d md5 entries", len(h.md5s))
	return h, rows.Err()
}

// Md5 returns the current md5 for a key ("" when the config does not exist).
func (h *ConfigHub) Md5(k ConfigKey) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.md5s[k]
}

// Publish updates the index and wakes listeners.
func (h *ConfigHub) Publish(k ConfigKey, md5v string) {
	h.mu.Lock()
	h.md5s[k] = md5v
	chs := h.takeWatchersLocked(k)
	h.mu.Unlock()
	notify(chs)
}

// Invalidate removes a deleted config from the index and wakes listeners.
func (h *ConfigHub) Invalidate(k ConfigKey) {
	h.mu.Lock()
	delete(h.md5s, k)
	chs := h.takeWatchersLocked(k)
	h.mu.Unlock()
	notify(chs)
}

func (h *ConfigHub) takeWatchersLocked(k ConfigKey) []chan struct{} {
	m := h.watchers[k]
	delete(h.watchers, k)
	chs := make([]chan struct{}, 0, len(m))
	for ch := range m {
		chs = append(chs, ch)
	}
	return chs
}

func notify(chs []chan struct{}) {
	for _, ch := range chs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WaitChanged parks until one of the listened keys changes (or the timeout
// expires) and returns the changed keys. known maps each key to the md5 the
// client last saw; a missing/"" current md5 means the config does not exist.
func (h *ConfigHub) WaitChanged(ctx context.Context, keys []ConfigKey, known map[ConfigKey]string, timeout time.Duration) []ConfigKey {
	if len(keys) == 0 {
		return nil
	}
	check := func() []ConfigKey {
		var changed []ConfigKey
		for _, k := range keys {
			if h.Md5(k) != known[k] {
				changed = append(changed, k)
			}
		}
		return changed
	}
	if ch := check(); len(ch) > 0 {
		return ch
	}
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	for _, k := range keys {
		m, ok := h.watchers[k]
		if !ok {
			m = map[chan struct{}]struct{}{}
			h.watchers[k] = m
		}
		m[ch] = struct{}{}
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		for _, k := range keys {
			if m, ok := h.watchers[k]; ok {
				delete(m, ch)
				if len(m) == 0 {
					delete(h.watchers, k)
				}
			}
		}
		h.mu.Unlock()
	}()

	// Wake on: write notification, 1s safety tick (multi-node writes land in
	// MySQL directly and only reach this node's index via the tick), timeout.
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-ch:
		case <-tick.C:
		case <-deadline.C:
			return nil
		case <-ctx.Done():
			return nil
		}
		if ch := check(); len(ch) > 0 {
			return ch
		}
	}
}
