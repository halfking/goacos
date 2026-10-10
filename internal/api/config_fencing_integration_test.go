//go:build integration

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

func activateConfig(t *testing.T, s *Server) {
	t.Helper()
	h, e := core.NewConfigHub(t.Context(), s.ST)
	if e != nil {
		t.Fatal(e)
	}
	s.Hub = h
}
func configHTTP(t *testing.T, s *Server, method, path, body string) (int, string) {
	t.Helper()
	token, _, _, e := s.Auth.Login(t.Context(), s.Cfg.AdminUsername, s.Cfg.AdminPassword)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}
func configSessionPID(t *testing.T, s *Server) int {
	t.Helper()
	var id int
	e := s.ST.WithConfigTransaction(t.Context(), func(ctx context.Context) error { return s.ST.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&id) })
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func TestSingleActiveConcurrentActivation(t *testing.T) {
	const n = 6
	stores := make([]*Server, n)
	for i := range stores {
		stores[i], _, _ = freshnessServer(t)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan bool, n)
	for _, s := range stores {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, e := core.NewConfigHub(t.Context(), s.ST); results <- e == nil }()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent database activation winners=%d", winners)
	}
	t.Logf("actual simultaneous activation sessions=%d winner=%d", n, winners)
}
func TestSingleActiveInFlightLossRollbackAndRestart(t *testing.T) {
	a, ns, _ := freshnessServer(t)
	activateConfig(t, a)
	key := core.ConfigKey{Tenant: ns, Group: "DEFAULT_GROUP", DataId: "rollback.yaml"}
	u := storage.ConfigUpsert{TenantId: ns, GroupId: key.Group, DataId: key.DataId, Content: "committed"}
	if _, _, e := a.ST.UpsertConfig(t.Context(), u); e != nil {
		t.Fatal(e)
	}
	pid := configSessionPID(t, a)
	written := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- a.ST.WithConfigTransaction(t.Context(), func(ctx context.Context) error {
			u.Content = "must-rollback"
			if _, _, e := a.ST.UpsertConfig(ctx, u); e != nil {
				return e
			}
			close(written)
			<-release
			return nil
		})
	}()
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not reach real transaction")
	}
	var killed bool
	if e := a.ST.DB.QueryRowContext(t.Context(), "SELECT pg_terminate_backend($1)", pid).Scan(&killed); e != nil || !killed {
		t.Fatalf("owned fixture session termination error=%v killed=%v", e, killed)
	}
	b, _, _ := freshnessServer(t)
	activateConfig(t, b)
	close(release)
	select {
	case e := <-result:
		if !errors.Is(e, storage.ErrConfigInactive) {
			t.Fatalf("old in-flight completion=%v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old write did not finish")
	}
	c, e := b.ST.GetConfig(t.Context(), key.DataId, key.Group, ns)
	if e != nil || c == nil || c.Content != "committed" {
		t.Fatalf("takeover observed rolled-back config: %v %v", c, e)
	}
	hs, total, e := b.ST.ListHistory(t.Context(), storage.HistoryListQuery{DataId: key.DataId, GroupId: key.Group, TenantId: ns, PageNo: 1, PageSize: 100})
	if e != nil || total != 1 || len(hs) != 1 || hs[0].Content != "committed" {
		t.Fatalf("config/history rollback mismatch total=%d rows=%v err=%v", total, hs, e)
	}
	if m, e := b.Hub.Md5(t.Context(), key); e != nil || m != md5Of("committed") {
		t.Fatalf("fresh preload/current DB MD5=%s err=%v", m, e)
	}
	if _, _, e := a.ST.UpsertConfig(t.Context(), u); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("old owner wrote after takeover: %v", e)
	}
	if _, e := a.ST.GetConfig(t.Context(), key.DataId, key.Group, ns); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("old owner read after takeover: %v", e)
	}
	if _, e := a.Hub.WaitChanged(t.Context(), []core.ConfigKey{key}, nil, time.Millisecond); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("old listener after takeover: %v", e)
	}
	if _, e := core.NewConfigHub(t.Context(), a.ST); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("lost Store silently reacquired=%v", e)
	}
	t.Log("actual PG backend loss aborted config+history before new owner admission; old owner permanently refused")
}
func TestSingleActiveListenersReleaseWriteConnectionAndLoseClosed(t *testing.T) {
	s, ns, _ := freshnessServer(t)
	activateConfig(t, s)
	k := core.ConfigKey{Tenant: ns, Group: "DEFAULT_GROUP", DataId: "listen.yaml"}
	u := storage.ConfigUpsert{TenantId: ns, GroupId: k.Group, DataId: k.DataId, Content: "before"}
	if _, _, e := s.ST.UpsertConfig(t.Context(), u); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := s.Hub.WaitChanged(ctx, []core.ConfigKey{k}, map[core.ConfigKey]string{k: md5Of("before")}, 10*time.Second)
		done <- e
	}()
	time.Sleep(50 * time.Millisecond)
	started := time.Now()
	u.Content = "after"
	if _, _, e := s.ST.UpsertConfig(t.Context(), u); e != nil {
		t.Fatal(e)
	}
	if time.Since(started) > time.Second {
		t.Fatal("long listener held write connection")
	}
	s.Hub.Publish(k, md5Of("after"))
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	done = make(chan error, 1)
	go func() {
		_, e := s.Hub.WaitChanged(ctx, []core.ConfigKey{k}, map[core.ConfigKey]string{k: md5Of("after")}, 10*time.Second)
		done <- e
	}()
	time.Sleep(50 * time.Millisecond)
	pid := configSessionPID(t, s)
	if _, e := s.ST.DB.ExecContext(t.Context(), "SELECT pg_terminate_backend($1)", pid); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if !errors.Is(e, storage.ErrConfigInactive) {
			t.Fatalf("lost connection listener leaked successful timeout/data: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener failed to detect connection loss")
	}
}
func TestSingleActiveMountedRouteFencesAndAtomicHistory(t *testing.T) {
	s, ns, _ := freshnessServer(t)
	activateConfig(t, s)
	q := url.Values{"tenant": {ns}, "group": {"DEFAULT_GROUP"}, "dataId": {"http.yaml"}}
	form := q.Clone()
	form.Set("content", "first")
	form.Set("config_tags", "one,two")
	for _, v := range []string{"v1", "v2"} {
		if code, body := configHTTP(t, s, "POST", map[string]string{"v1": "/nacos/v1/cs/configs", "v2": "/nacos/v2/cs/config"}[v], form.Encode()); code != 200 {
			t.Fatalf("publish%s %d %s", v, code, body)
		}
	}
	c, e := s.ST.GetConfig(t.Context(), "http.yaml", "DEFAULT_GROUP", ns)
	if e != nil || c == nil {
		t.Fatal(e)
	}
	hs, total, e := s.ST.ListHistory(t.Context(), storage.HistoryListQuery{TenantId: ns, DataId: "http.yaml", GroupId: "DEFAULT_GROUP", PageNo: 1, PageSize: 100})
	if e != nil || total != 2 {
		t.Fatalf("history total=%d err=%v", total, e)
	}
	if h, e := s.ST.GetHistory(t.Context(), hs[0].Nid); e != nil || h == nil {
		t.Fatal(e)
	}
	if _, e := s.ST.GetPreviousHistory(t.Context(), "http.yaml", "DEFAULT_GROUP", ns, hs[0].Nid); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.ST.ListConfigs(t.Context(), storage.ListQuery{TenantId: ns, Tag: "one", PageNo: 1, PageSize: 100}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ST.CountConfigs(t.Context()); e != nil {
		t.Fatal(e)
	}
	// Actual API publish fails after its config+history writes, at tag insertion.
	if _, e := s.ST.DB.ExecContext(t.Context(), `CREATE OR REPLACE FUNCTION w038_reject_tag() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.tag_name='w038-reject' THEN RAISE EXCEPTION 'owned failure injection'; END IF; RETURN NEW; END $$; CREATE TRIGGER w038_reject_tag BEFORE INSERT ON config_tags_relation FOR EACH ROW EXECUTE FUNCTION w038_reject_tag()`); e != nil {
		t.Fatal(e)
	}
	defer s.ST.DB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS w038_reject_tag ON config_tags_relation; DROP FUNCTION IF EXISTS w038_reject_tag()`)
	form.Set("content", "must-rollback")
	form.Set("config_tags", "w038-reject")
	if code, _ := configHTTP(t, s, "POST", "/nacos/v1/cs/configs", form.Encode()); code != 500 {
		t.Fatalf("tag failure status=%d", code)
	}
	c, e = s.ST.GetConfig(t.Context(), "http.yaml", "DEFAULT_GROUP", ns)
	if e != nil || c.Content != "first" {
		t.Fatalf("error committed content=%v %v", c, e)
	}
	_, total, e = s.ST.ListHistory(t.Context(), storage.HistoryListQuery{TenantId: ns, DataId: "http.yaml", GroupId: "DEFAULT_GROUP", PageNo: 1, PageSize: 100})
	if e != nil || total != 2 {
		t.Fatalf("error committed history total=%d err=%v", total, e)
	}
	if code, body := configHTTP(t, s, "GET", "/nacos/v1/cs/configs?"+q.Encode(), ""); code != 200 || body != "first" {
		t.Fatalf("raw compatible config=%d %s", code, body)
	}
	// Long listener uses HTTP and must refuse a lost active session.
	pid := configSessionPID(t, s)
	if _, e := s.ST.DB.ExecContext(t.Context(), "SELECT pg_terminate_backend($1)", pid); e != nil {
		t.Fatal(e)
	}
	paths := []struct{ method, path, body string }{
		{"GET", "/nacos/v1/cs/configs?" + q.Encode(), ""}, {"GET", "/nacos/v1/cs/configs?search=accurate&" + q.Encode(), ""}, {"POST", "/nacos/v1/cs/configs", form.Encode()}, {"DELETE", "/nacos/v1/cs/configs?" + q.Encode(), ""}, {"GET", "/nacos/v1/cs/history?" + q.Encode(), ""}, {"GET", "/nacos/v1/cs/history/configId?nid=1", ""}, {"POST", "/nacos/v1/cs/configs/listener", url.Values{"Listening-Configs": {"http.yaml\x02DEFAULT_GROUP\x02" + md5Of("first") + "\x02" + ns + "\x01"}}.Encode()},
		{"GET", "/nacos/v2/cs/config?" + q.Encode(), ""}, {"GET", "/nacos/v2/cs/config/list?" + q.Encode(), ""}, {"GET", "/nacos/v1/cs/history/configs?" + q.Encode() + "&nid=1", ""}, {"POST", "/nacos/v2/cs/config", form.Encode()}, {"DELETE", "/nacos/v2/cs/config?" + q.Encode(), ""}, {"GET", "/nacos/v2/cs/history/list?" + q.Encode(), ""}, {"GET", "/nacos/v2/cs/history?nid=1", ""}, {"POST", "/nacos/v2/cs/config/listener", form.Encode()},
	}
	for _, p := range paths {
		t.Run(p.method+"-"+p.path, func(t *testing.T) {
			if code, _ := configHTTP(t, s, p.method, p.path, p.body); code != 503 {
				t.Fatalf("lost owner route admitted HTTP %d", code)
			}
		})
	}
	b, _, _ := freshnessServer(t)
	activateConfig(t, b)
	if code, body := configHTTP(t, b, "GET", "/nacos/v1/cs/configs?"+q.Encode(), ""); code != 200 || body != "first" {
		t.Fatalf("replacement config=%d %s", code, body)
	}
	if ok, e := b.ST.DeleteConfig(t.Context(), "http.yaml", "DEFAULT_GROUP", ns, "owned-test", ""); e != nil || !ok {
		t.Fatalf("delete=%v %v", ok, e)
	}
	b.Hub.Invalidate(core.ConfigKey{Tenant: ns, Group: "DEFAULT_GROUP", DataId: "http.yaml"})
	if _, total, e := b.ST.ListHistory(t.Context(), storage.HistoryListQuery{TenantId: ns, DataId: "http.yaml", GroupId: "DEFAULT_GROUP", PageNo: 1, PageSize: 100}); e != nil || total != 3 {
		t.Fatalf("delete history total=%d %v", total, e)
	}
}
func TestSingleActiveResponseRollbackAndBounds(t *testing.T) {
	s, ns, _ := freshnessServer(t)
	activateConfig(t, s)
	for _, status := range []int{400, 500} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/nacos/v1/cs/configs", nil)
		s.serveConfig(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, _, e := s.ST.UpsertConfig(r.Context(), storage.ConfigUpsert{DataId: "reject", GroupId: "g", TenantId: ns, Content: "reject"}); e != nil {
				t.Fatal(e)
			}
			writeText(w, status, "rejected")
		}))
		if w.Code != status {
			t.Fatalf("response=%d", w.Code)
		}
		if c, e := s.ST.GetConfig(t.Context(), "reject", "g", ns); e != nil || c != nil {
			t.Fatalf("handler rejection committed %v %v", c, e)
		}
	}
	b := &configResponse{headers: make(http.Header)}
	_, e := b.Write(make([]byte, 16*1024*1024+1))
	if !errors.Is(e, errConfigHandler) || !b.overflow {
		t.Fatal("unbounded buffered response")
	}
	// Cancellation of a waiting listener is meaningful and leaves writes usable.
	k := core.ConfigKey{Tenant: ns, Group: "g", DataId: "absent"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := s.Hub.WaitChanged(ctx, []core.ConfigKey{k}, nil, time.Second); e == nil {
		t.Fatal("cancelled listener admitted")
	}
}

func TestSingleActiveMetricsRefusesInactive(t *testing.T) {
	s, _, _ := freshnessServer(t)
	if code, _ := configHTTP(t, s, "GET", "/nacos/v1/ns/operator/metrics", ""); code != 503 {
		t.Fatalf("inactive config metrics manufactured a successful count: HTTP %d", code)
	}
	activateConfig(t, s)
	if code, _ := configHTTP(t, s, "GET", "/nacos/v1/ns/operator/metrics", ""); code != 200 {
		t.Fatalf("active config metrics HTTP %d", code)
	}
}
func TestSingleActiveDeleteTagFailureRollsBackWithoutLosingOwner(t *testing.T) {
	s, ns, _ := freshnessServer(t)
	activateConfig(t, s)
	id, _, e := s.ST.UpsertConfig(t.Context(), storage.ConfigUpsert{DataId: "delete-fail", GroupId: "g", TenantId: ns, Content: "keep"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ST.ReplaceConfigTags(t.Context(), id, "delete-fail", "g", ns, []string{"w038-delete-reject"}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ST.DB.ExecContext(t.Context(), `CREATE OR REPLACE FUNCTION w038_reject_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD.tag_name='w038-delete-reject' THEN RAISE EXCEPTION 'owned delete failure'; END IF; RETURN OLD; END $$; CREATE TRIGGER w038_reject_delete BEFORE DELETE ON config_tags_relation FOR EACH ROW EXECUTE FUNCTION w038_reject_delete()`); e != nil {
		t.Fatal(e)
	}
	defer s.ST.DB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS w038_reject_delete ON config_tags_relation; DROP FUNCTION IF EXISTS w038_reject_delete()`)
	if _, e = s.ST.DeleteConfig(t.Context(), "delete-fail", "g", ns, "", ""); e == nil {
		t.Fatal("failed tag delete accepted")
	}
	c, e := s.ST.GetConfig(t.Context(), "delete-fail", "g", ns)
	if e != nil || c == nil || c.Content != "keep" {
		t.Fatalf("SQL failure unnecessarily loses owner or commits delete: %v %v", c, e)
	}
	if _, n, e := s.ST.ListHistory(t.Context(), storage.HistoryListQuery{TenantId: ns, DataId: "delete-fail", GroupId: "g"}); e != nil || n != 1 {
		t.Fatalf("delete failure history=%d err=%v", n, e)
	}
}

func TestSingleActiveNamespaceConfigCountsFailClosed(t *testing.T) {
	s, ns, _ := freshnessServer(t)
	if _, e := s.ST.ListNamespaces(t.Context()); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("namespace config counts bypass active DB fence: %v", e)
	}
	if _, _, e := s.ST.DeleteNamespace(t.Context(), ns); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("namespace delete config precondition bypasses active DB fence: %v", e)
	}
	for _, path := range []string{"/nacos/v1/console/namespaces", "/nacos/v2/console/namespace/list"} {
		if code, _ := configHTTP(t, s, "GET", path, ""); code != 503 {
			t.Fatalf("inactive namespace count route=%d", code)
		}
	}
	activateConfig(t, s)
	if ok, e := s.ST.CreateNamespace(t.Context(), ns, "owned", "synthetic"); e != nil || !ok {
		t.Fatal(e)
	}
	if _, e := s.ST.ListNamespaces(t.Context()); e != nil {
		t.Fatal(e)
	}
	if ok, _, e := s.ST.DeleteNamespace(t.Context(), ns); e != nil || !ok {
		t.Fatal(e)
	}
}

func TestSingleActiveListenerMethodCompatibility(t *testing.T) {
	s, _, _ := freshnessServer(t)
	activateConfig(t, s)
	if code, _ := configHTTP(t, s, "GET", "/nacos/v1/cs/configs/listener", ""); code != 405 {
		t.Fatalf("active listener method rejection changed to HTTP %d", code)
	}
}

func TestSingleActivePoolCannotStarveAuth(t *testing.T) {
	s, _, _ := freshnessServer(t)
	cfg := *s.Cfg
	cfg.MaxOpenConns = 1
	cfg.MaxIdleConns = 1
	st, e := storage.Open(t.Context(), &cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if _, e = core.NewConfigHub(t.Context(), st); !errors.Is(e, storage.ErrConfigInactive) {
		t.Fatalf("reserved config session consumes sole auth/naming connection: %v", e)
	}
}
