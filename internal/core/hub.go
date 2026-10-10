// Package core holds in-process services shared by the API handlers.
package core

import (
	"context"
	"github.com/halfking/goacos/internal/storage"
	"log"
	"sync"
	"time"
)

type ConfigKey struct{ Tenant, Group, DataId string }

// Local notifications accelerate waits; only the owned database transaction
// may decide current values. The preload is not a cross-node cache authority.
type ConfigHub struct {
	mu       sync.Mutex
	md5s     map[ConfigKey]string
	watchers map[ConfigKey]map[chan struct{}]struct{}
	store    *storage.Store
}

func NewConfigHub(ctx context.Context, st *storage.Store) (*ConfigHub, error) {
	if st == nil {
		return nil, storage.ErrConfigInactive
	}
	if e := st.AcquireConfigOwnership(ctx); e != nil {
		return nil, e
	}
	h := &ConfigHub{md5s: map[ConfigKey]string{}, watchers: map[ConfigKey]map[chan struct{}]struct{}{}, store: st}
	entries, e := st.ConfigMD5Index(ctx)
	if e != nil {
		_ = st.ReleaseConfigOwnership()
		return nil, e
	}
	for _, v := range entries {
		h.md5s[ConfigKey{v.Tenant, v.Group, v.DataID}] = v.MD5
	}
	log.Printf("[goacos] database-active config hub preloaded %d entries", len(entries))
	return h, nil
}
func (h *ConfigHub) Md5(ctx context.Context, k ConfigKey) (string, error) {
	if h == nil || h.store == nil {
		return "", storage.ErrConfigInactive
	}
	return h.store.ConfigMD5(ctx, k.DataId, k.Group, k.Tenant)
}
func (h *ConfigHub) Publish(k ConfigKey, md5 string) {
	h.mu.Lock()
	h.md5s[k] = md5
	chs := h.takeWatchersLocked(k)
	h.mu.Unlock()
	notify(chs)
}
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
	out := make([]chan struct{}, 0, len(m))
	for ch := range m {
		out = append(out, ch)
	}
	return out
}
func notify(chs []chan struct{}) {
	for _, ch := range chs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (h *ConfigHub) WaitChanged(ctx context.Context, keys []ConfigKey, known map[ConfigKey]string, timeout time.Duration) ([]ConfigKey, error) {
	if h == nil || h.store == nil {
		return nil, storage.ErrConfigInactive
	}
	check := func() (changed []ConfigKey, e error) {
		e = h.store.WithConfigTransaction(ctx, func(ctx context.Context) error {
			for _, k := range keys {
				md5, e := h.Md5(ctx, k)
				if e != nil {
					return e
				}
				if md5 != known[k] {
					changed = append(changed, k)
				}
			}
			return nil
		})
		return
	}
	if changed, e := check(); e != nil || len(changed) > 0 || len(keys) == 0 {
		return changed, e
	}
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	for _, k := range keys {
		m := h.watchers[k]
		if m == nil {
			m = map[chan struct{}]struct{}{}
			h.watchers[k] = m
		}
		m[ch] = struct{}{}
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, k := range keys {
			if m := h.watchers[k]; m != nil {
				delete(m, ch)
				if len(m) == 0 {
					delete(h.watchers, k)
				}
			}
		}
	}()
	// Registration recheck closes the missed-notification window. No connection
	// or ownership semaphore is held while the long listener parks.
	if changed, e := check(); e != nil || len(changed) > 0 {
		return changed, e
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-ch:
		case <-tick.C:
		case <-h.store.ConfigOwnershipDone():
			return nil, storage.ErrConfigInactive
		case <-deadline.C:
			return check()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if changed, e := check(); e != nil || len(changed) > 0 {
			return changed, e
		}
	}
}
