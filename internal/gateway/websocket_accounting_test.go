package gateway

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Observe the actual Forward call, not an assumption about ReverseProxy.
// One live upgraded connection on the actual first instance must remain counted,
// route ordinary requests elsewhere, and release only when forwarding ends.
func TestWebSocketCountLivesUntilForwardReturns(t *testing.T) {
	backend := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ws" {
				_, _ = io.WriteString(w, name)
				return
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("backend hijack: %v", err)
				return
			}
			defer conn.Close()
			digest := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
			if err == nil {
				_, err = rw.Write([]byte{0x81, 1, name[0]})
			}
			if err == nil {
				err = rw.Flush()
			}
			if err != nil {
				t.Errorf("backend handshake: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, conn)
		}))
	}
	a, b := backend("A"), backend("B")
	defer a.Close()
	defer b.Close()
	tbl := failoverTable(t, "least_conn", a.URL, b.URL)
	g := testGateway(tbl, nil)
	target := tbl.Lookup("acc.example.com", "/ws", nil)
	first, other := "A", "B"
	if "http://"+addr(target.Instances[0]) != a.URL {
		first, other = other, first
	}
	returned := make(chan struct{}, 1)
	ordinaryDone := make(chan struct{}, 1)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.dispatch(w, r, false)
		if r.URL.Path == "/ws" {
			returned <- struct{}{}
		} else {
			ordinaryDone <- struct{}{}
		}
	}))
	defer front.Close()
	get := func(want string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/", nil)
		req.Host = "acc.example.com"
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || string(body) != want {
			t.Fatalf("HTTP want %s: status=%d body=%q error=%v", want, resp.StatusCode, body, err)
		}
		select {
		case <-ordinaryDone:
		case <-time.After(3 * time.Second):
			t.Fatal("ordinary Forward did not finish")
		}
	}
	get(first) // baseline follows actual table order, not random backend ports
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(front.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: acc.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upgrade status=%d body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("invalid WebSocket handshake accept: %q", got)
	}
	frame := make([]byte, 3)
	if _, err = io.ReadFull(rd, frame); err != nil || string(frame) != string([]byte{0x81, 1, first[0]}) {
		t.Fatalf("frame=%q error=%v", frame, err)
	}
	state := g.picker.state(targetKey(target))
	count := func() int {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.inflight[addr(target.Instances[0])]
	}
	if got := count(); got != 1 {
		t.Fatalf("live upgraded first-instance count=%d, want 1", got)
	}
	select {
	case <-returned:
		t.Fatal("Forward already returned while the upgraded connection is live")
	default:
	}
	for i := 0; i < 6; i++ {
		get(other) // unequal load is essential; A/B ties cannot answer this question
	}
	if got := count(); got != 1 {
		t.Fatalf("ordinary traffic changed upgraded first-instance count to %d", got)
	}
	conn.Close()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("Forward did not return after client connection closed")
	}
	if got := count(); got != 0 {
		t.Fatalf("after Forward returned first-instance count=%d, want 0", got)
	}
	get(first) // closing the upgrade restores the original tie
}
