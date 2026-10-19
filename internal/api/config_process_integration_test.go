//go:build integration

package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/core"
	"github.com/halfking/goacos/internal/storage"
)

func TestSingleActiveProcessHelper(t *testing.T) {
	if os.Getenv("GOACOS_SINGLE_ACTIVE_HELPER") != "1" {
		return
	}
	dsn := os.Getenv("GOACOS_TEST_PG_DSN")
	u, e := url.Parse(dsn)
	if e != nil || u == nil || u.Hostname() != "127.0.0.1" || !strings.HasPrefix(u.Path, "/w038_") || u.User == nil {
		fmt.Println("fixture-invalid")
		os.Exit(2)
	}
	pw, _ := u.User.Password()
	cfg := &config.Config{DBType: "postgres", DBDSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2, AuthEnabled: true, AdminUsername: "w038-test-admin", AdminPassword: pw}
	st, e := storage.Open(context.Background(), cfg)
	if e != nil {
		fmt.Println("storage-refused")
		os.Exit(2)
	}
	defer st.Close()
	h, e := core.NewConfigHub(context.Background(), st)
	if e != nil {
		fmt.Println("inactive")
		os.Exit(3)
	}
	ns := os.Getenv("GOACOS_SINGLE_ACTIVE_PROCESS_NS")
	k := core.ConfigKey{Tenant: ns, Group: "g", DataId: "process.yaml"}
	c, e := st.GetConfig(context.Background(), k.DataId, k.Group, ns)
	if e != nil {
		os.Exit(2)
	}
	if c == nil {
		if _, _, e = st.UpsertConfig(context.Background(), storage.ConfigUpsert{DataId: k.DataId, GroupId: k.Group, TenantId: ns, Content: "durable-process"}); e != nil {
			os.Exit(2)
		}
	} else if c.Content != "durable-process" {
		os.Exit(2)
	}
	if _, e = h.Md5(context.Background(), k); e != nil {
		os.Exit(2)
	}
	if _, n, e := st.ListHistory(context.Background(), storage.HistoryListQuery{TenantId: ns, GroupId: k.Group, DataId: k.DataId}); e != nil || n != 1 {
		os.Exit(2)
	}
	fmt.Println("active:durable-config-history")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
func TestSingleActiveTwoProcessesCrashAndRestart(t *testing.T) {
	_, _, _ = freshnessServer(t) // prove explicit fixture/auth setup before subprocesses
	ns := "w038-process-" + storage.NewID()
	env := append(os.Environ(), "GOACOS_SINGLE_ACTIVE_HELPER=1", "GOACOS_SINGLE_ACTIVE_PROCESS_NS="+ns)
	start := func() (*exec.Cmd, *bufio.Reader) {
		c := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSingleActiveProcessHelper$")
		c.Env = env
		in, e := c.StdinPipe()
		if e != nil {
			t.Fatal(e)
		}
		out, e := c.StdoutPipe()
		if e != nil {
			t.Fatal(e)
		}
		c.Stderr = io.Discard
		if e = c.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { in.Close(); c.Process.Kill(); c.Wait() })
		return c, bufio.NewReader(out)
	}
	read := func(r *bufio.Reader) string {
		ch := make(chan string, 1)
		go func() { s, _ := r.ReadString('\n'); ch <- strings.TrimSpace(s) }()
		select {
		case s := <-ch:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("helper admission timed out")
			return ""
		}
	}
	first, r := start()
	if s := read(r); s != "active:durable-config-history" {
		t.Fatalf("first process=%s", s)
	}
	second, r := start()
	if s := read(r); s != "inactive" {
		t.Fatalf("second process must refuse database owner, got %s", s)
	}
	if e := second.Wait(); e == nil {
		t.Fatal("standby process succeeded")
	}
	if e := first.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e := first.Wait(); e == nil {
		t.Fatal("expected crash signal")
	}
	_, r = start()
	if s := read(r); s != "active:durable-config-history" {
		t.Fatalf("restart did not reload committed history/config=%s", s)
	}
	t.Log("actual separate OS processes: second activation refused; SIGKILL released DB session; restart reloaded committed config and single history row")
}
