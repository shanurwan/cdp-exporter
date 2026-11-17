package cdp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Events receives what a Watcher observes. Implementations must be safe for
// concurrent use; one Events value is shared by all watchers.
type Events interface {
	TargetCreated(targetType string)
	TargetDestroyed(targetType string)
	TargetCrashed(targetType, status string)
	Reconnect()
}

// Watcher keeps one browser-level websocket open and turns Target domain
// events into counters. It only listens: it calls Target.setDiscoverTargets,
// never attaches to a target, and never changes browser state.
type Watcher struct {
	Base   string
	Client *Client
	Events Events
	Log    *slog.Logger

	mu        sync.Mutex
	connected bool
	targets   map[string]targetInfo
}

type targetInfo struct {
	Type      string
	ContextID string
}

// State is what a connected watcher currently knows.
type State struct {
	Connected bool
	Contexts  int // distinct browser contexts that own at least one target
	ByType    map[string]int
}

// State returns a copy of the watcher's live view.
func (w *Watcher) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := State{Connected: w.connected, ByType: map[string]int{}}
	ctxs := map[string]bool{}
	for _, t := range w.targets {
		st.ByType[t.Type]++
		if t.ContextID != "" {
			ctxs[t.ContextID] = true
		}
	}
	st.Contexts = len(ctxs)
	return st
}

// Run connects and reconnects until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	first := true
	for ctx.Err() == nil {
		if !first {
			w.Events.Reconnect()
		}
		first = false
		started := time.Now()
		err := w.session(ctx)
		w.mu.Lock()
		w.connected = false
		w.targets = nil
		w.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 30*time.Second {
			backoff = 500 * time.Millisecond
		}
		w.Log.Debug("cdp watch disconnected", "endpoint", w.Base, "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

type message struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type wireTargetInfo struct {
	TargetID         string `json:"targetId"`
	Type             string `json:"type"`
	BrowserContextID string `json:"browserContextId"`
}

func (w *Watcher) session(ctx context.Context) error {
	v, _, err := w.Client.Version(ctx, w.Base)
	if err != nil {
		return err
	}
	wsURL, err := BrowserWebSocketURL(w.Base, v)
	if err != nil {
		return err
	}
	u, _ := url.Parse(wsURL)
	hdr := http.Header{}
	hdr.Set("Host", hostHeader(u))
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(16 << 20)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Target.setDiscoverTargets", "params": map[string]any{"discover": true}}); err != nil {
		return err
	}
	w.mu.Lock()
	w.targets = map[string]targetInfo{}
	w.mu.Unlock()

	// Chrome reports every existing target as targetCreated before it answers
	// setDiscoverTargets. Those are not new, so they are not counted.
	ready := false
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.ID == 1 {
			if m.Error != nil {
				return &protocolError{m.Error.Message}
			}
			ready = true
			w.mu.Lock()
			w.connected = true
			w.mu.Unlock()
			continue
		}
		switch m.Method {
		case "Target.targetCreated", "Target.targetInfoChanged":
			var p struct {
				TargetInfo wireTargetInfo `json:"targetInfo"`
			}
			if json.Unmarshal(m.Params, &p) != nil {
				continue
			}
			w.mu.Lock()
			_, known := w.targets[p.TargetInfo.TargetID]
			w.targets[p.TargetInfo.TargetID] = targetInfo{Type: p.TargetInfo.Type, ContextID: p.TargetInfo.BrowserContextID}
			w.mu.Unlock()
			if m.Method == "Target.targetCreated" && ready && !known {
				w.Events.TargetCreated(p.TargetInfo.Type)
			}
		case "Target.targetDestroyed":
			var p struct {
				TargetID string `json:"targetId"`
			}
			if json.Unmarshal(m.Params, &p) != nil {
				continue
			}
			w.mu.Lock()
			t, ok := w.targets[p.TargetID]
			delete(w.targets, p.TargetID)
			w.mu.Unlock()
			if ok {
				w.Events.TargetDestroyed(t.Type)
			}
		case "Target.targetCrashed":
			var p struct {
				TargetID string `json:"targetId"`
				Status   string `json:"status"`
			}
			if json.Unmarshal(m.Params, &p) != nil {
				continue
			}
			w.mu.Lock()
			t := w.targets[p.TargetID]
			w.mu.Unlock()
			w.Events.TargetCrashed(t.Type, p.Status)
		}
	}
}

type protocolError struct{ msg string }

func (e *protocolError) Error() string { return "cdp: " + e.msg }
