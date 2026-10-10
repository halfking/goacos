package core

import (
	"context"
	"errors"
	"github.com/halfking/goacos/internal/storage"
	"testing"
	"time"
)

func TestConfigHubWithoutDatabaseFailsClosed(t *testing.T) {
	h := &ConfigHub{md5s: map[ConfigKey]string{{Group: "G", DataId: "D"}: "forged"}, watchers: map[ConfigKey]map[chan struct{}]struct{}{}}
	if _, e := h.Md5(context.Background(), ConfigKey{Group: "G", DataId: "D"}); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatal("memory established configuration ownership")
	}
	if _, e := h.WaitChanged(context.Background(), []ConfigKey{{Group: "G", DataId: "D"}}, nil, time.Millisecond); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatal("unowned listener admitted")
	}
	if _, e := NewConfigHub(context.Background(), nil); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatal(e)
	}
}
