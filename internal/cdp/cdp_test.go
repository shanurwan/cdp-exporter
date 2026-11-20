package cdp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeChrome mimics the parts of Chrome's DevTools server the exporter uses,
// including its Host-header check.
type fakeChrome struct {
	srv   *httptest.Server
	conns chan *websocket.Conn
}

func newFakeChrome(t *testing.T) *fakeChrome {
	f := &fakeChrome{conns: make(chan *websocket.Conn, 1)}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	checkHost := func(w http.ResponseWriter, r *http.Request) bool {
		h := r.Host
		if i := strings.LastIndexByte(h, ':'); i > 0 {
			h = h[:i]
		}
		if h != "localhost" && h != "127.0.0.1" {
			http.Error(w, "Host header is specified and is not an IP address or localhost.", http.StatusInternalServerError)
			return false
		}
		return true
	}
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		if !checkHost(w, r) {
			return
		}
		json.NewEncoder(w).Encode(Version{Browser: "HeadlessChrome/142.0", WebSocketDebuggerURL: "ws://localhost/devtools/browser/abc"})
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		if !checkHost(w, r) {
			return
		}
		json.NewEncoder(w).Encode([]Target{{ID: "1", Type: "page"}, {ID: "2", Type: "service_worker"}})
	})
	mux.HandleFunc("/devtools/browser/abc", func(w http.ResponseWriter, r *http.Request) {
		if !checkHost(w, r) {
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.conns <- c
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestHostHeaderRewrite(t *testing.T) {
	f := newFakeChrome(t)
	// Reach the fake through a DNS name, as in Kubernetes ("http://chrome:9222").
	base := strings.Replace(f.srv.URL, "127.0.0.1", "chrome.invalid", 1)
	c := NewClient(2 * time.Second)
	c.HTTP.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, strings.TrimPrefix(f.srv.URL, "http://"))
	}}
	v, _, err := c.Version(context.Background(), base)
	if err != nil {
		t.Fatalf("Version through a DNS name: %v", err)
	}
	ws, err := BrowserWebSocketURL(base, v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ws, "ws://chrome.invalid:") || !strings.HasSuffix(ws, "/devtools/browser/abc") {
		t.Errorf("websocket URL = %q", ws)
	}
}

type recorder struct {
	mu                         sync.Mutex
	created, destroyed, rcount int
	crashes                    []string
}

func (r *recorder) TargetCreated(string)   { r.mu.Lock(); r.created++; r.mu.Unlock() }
func (r *recorder) TargetDestroyed(string) { r.mu.Lock(); r.destroyed++; r.mu.Unlock() }
func (r *recorder) TargetCrashed(typ, status string) {
	r.mu.Lock()
	r.crashes = append(r.crashes, typ+"/"+status)
	r.mu.Unlock()
}
func (r *recorder) Reconnect() { r.mu.Lock(); r.rcount++; r.mu.Unlock() }

func TestWatcherCountsEvents(t *testing.T) {
	f := newFakeChrome(t)
	rec := &recorder{}
	w := &Watcher{Base: f.srv.URL, Client: NewClient(2 * time.Second), Events: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	var conn *websocket.Conn
	select {
	case conn = <-f.conns:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not connect")
	}
	var req map[string]any
	if err := conn.ReadJSON(&req); err != nil || req["method"] != "Target.setDiscoverTargets" {
		t.Fatalf("first message = %v, %v", req, err)
	}
	send := func(v string) {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	// On the first session, a target that existed before the exporter found
	// the browser counts as created.
	send(`{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"old","type":"page","browserContextId":"C1"}}}`)
	send(`{"id":1,"result":{}}`)
	send(`{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"t1","type":"page","browserContextId":"C2"}}}`)
	send(`{"method":"Target.targetCrashed","params":{"targetId":"t1","status":"oom","errorCode":-1}}`)
	send(`{"method":"Target.targetDestroyed","params":{"targetId":"t1"}}`)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			rec.mu.Lock()
			ok := cond()
			rec.mu.Unlock()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				rec.mu.Lock()
				defer rec.mu.Unlock()
				t.Fatalf("%s: created=%d destroyed=%d reconnects=%d crashes=%v", what, rec.created, rec.destroyed, rec.rcount, rec.crashes)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("first session", func() bool { return rec.destroyed == 1 })
	rec.mu.Lock()
	if rec.created != 2 || rec.rcount != 0 || len(rec.crashes) != 1 || rec.crashes[0] != "page/oom" {
		t.Errorf("created=%d reconnects=%d crashes=%v, want 2, 0, [page/oom]", rec.created, rec.rcount, rec.crashes)
	}
	rec.mu.Unlock()
	if st := w.State(); !st.Connected || st.Contexts != 1 || st.ByType["page"] != 1 {
		t.Errorf("state = %+v, want connected with 1 context (C1)", st)
	}

	// The browser drops the socket and comes back. The reconnect counts once,
	// and the target it reports again is not created a second time.
	conn.Close()
	select {
	case conn = <-f.conns:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not reconnect")
	}
	if err := conn.ReadJSON(&req); err != nil {
		t.Fatal(err)
	}
	send(`{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"old","type":"page","browserContextId":"C1"}}}`)
	send(`{"id":1,"result":{}}`)
	waitFor("reconnect", func() bool { return rec.rcount == 1 })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.created != 2 {
		t.Errorf("created=%d after reconnect, want 2", rec.created)
	}
}
