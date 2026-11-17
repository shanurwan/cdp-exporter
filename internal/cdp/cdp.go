// Package cdp talks to a Chrome DevTools Protocol endpoint: the HTTP discovery
// API (/json/version, /json/list) and a browser-level websocket used to watch
// target events.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Version is the /json/version response.
type Version struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Target is one /json/list entry.
type Target struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Client calls the HTTP discovery API.
type Client struct {
	HTTP *http.Client
}

// NewClient returns a client with a short timeout; CDP discovery calls are
// local and should answer in milliseconds.
func NewClient(timeout time.Duration) *Client {
	return &Client{HTTP: &http.Client{Timeout: timeout}}
}

// Version calls /json/version and returns how long it took.
func (c *Client) Version(ctx context.Context, base string) (*Version, time.Duration, error) {
	var v Version
	start := time.Now()
	err := c.get(ctx, base, "/json/version", &v)
	return &v, time.Since(start), err
}

// Targets calls /json/list.
func (c *Client) Targets(ctx context.Context, base string) ([]Target, error) {
	var ts []Target
	err := c.get(ctx, base, "/json/list", &ts)
	return ts, err
}

func (c *Client) get(ctx context.Context, base, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Host = hostHeader(req.URL)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	return json.Unmarshal(body, into)
}

// hostHeader returns a Host header Chrome accepts. Since Chrome 66 the
// DevTools HTTP server rejects a Host that is not an IP address or
// "localhost" (DNS-rebinding protection), which breaks the usual Kubernetes
// address "http://chrome:9222". The connection still goes to the real host.
func hostHeader(u *url.URL) string {
	h := u.Hostname()
	if h == "localhost" || net.ParseIP(h) != nil {
		return u.Host
	}
	if p := u.Port(); p != "" {
		return "localhost:" + p
	}
	return "localhost"
}

// BrowserWebSocketURL returns the browser websocket URL reported by Chrome,
// with its host replaced by the host the exporter actually reached. Chrome
// fills it from the Host header, which may be "localhost".
func BrowserWebSocketURL(base string, v *Version) (string, error) {
	if v.WebSocketDebuggerURL == "" {
		return "", errors.New("no webSocketDebuggerUrl in /json/version")
	}
	ws, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil {
		return "", err
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ws.Host = b.Host
	if b.Scheme == "https" {
		ws.Scheme = "wss"
	} else {
		ws.Scheme = "ws"
	}
	return ws.String(), nil
}
