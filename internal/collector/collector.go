// Package collector turns /proc, profile-directory and CDP observations into
// Prometheus metrics.
package collector

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/shanurwan/cdp-exporter/internal/cdp"
	"github.com/shanurwan/cdp-exporter/internal/procscan"
	"github.com/shanurwan/cdp-exporter/internal/profiles"
)

const ns = "chrome"

// Config configures a Collector.
type Config struct {
	Proc     *procscan.Scanner
	Profiles *profiles.Scanner
	CDP      *cdp.Client
	// Static are CDP endpoints given on the command line, e.g.
	// http://127.0.0.1:9222. They get a per-endpoint chrome_cdp_up series.
	Static []string
	// Discover finds the debugging port of every browser process from its
	// --remote-debugging-port flag or its DevToolsActivePort file.
	Discover     bool
	DiscoverHost string
	// Watch keeps a websocket open to every reachable endpoint to count
	// target crashes and browser contexts.
	Watch   bool
	Timeout time.Duration
	Log     *slog.Logger
}

// Collector implements prometheus.Collector.
type Collector struct {
	cfg Config
	ctx context.Context

	mu       sync.Mutex // serialises scrapes
	watchMu  sync.Mutex
	watchers map[string]*watch

	created, destroyed *prometheus.CounterVec
	crashes            *prometheus.CounterVec
	reconnects         prometheus.Counter
	rtt                prometheus.Histogram
	scrapeErrors       *prometheus.CounterVec
}

type watch struct {
	w      *cdp.Watcher
	cancel context.CancelFunc
	source string
}

var (
	dBrowsers       = desc("browsers", "Running Chrome browser processes (each is one browser instance or session).")
	dProcs          = desc("processes", "Running Chrome processes by --type (browser, renderer, zygote, gpu-process, utility, ...).", "type")
	dRSS            = desc("process_resident_memory_bytes", "Sum of RSS of Chrome processes by type. Shared pages are counted once per process, so this overstates real use; see the proportional metric.", "type")
	dPSS            = desc("process_proportional_memory_bytes", "Sum of PSS of Chrome processes by type, for processes whose smaps_rollup is readable.", "type")
	dPSSUnreadable  = desc("process_pss_unreadable", "Chrome processes whose smaps_rollup could not be read (PSS missing from the proportional metric).")
	dBrowserMax     = desc("browser_memory_max_bytes", "Memory of the largest browser together with all its descendants (PSS where readable, otherwise RSS).")
	dBrowserOldest  = desc("browser_oldest_age_seconds", "Age of the oldest running browser process. A browser that never goes away is usually a leaked session.")
	dOrphans        = desc("orphan_processes", "Chrome child processes (renderer, zygote, utility, gpu) whose browser is gone. Chrome never cleans these up.", "type")
	dOrphanRSS      = desc("orphan_resident_memory_bytes", "RSS held by orphaned Chrome child processes.")
	dDefunct        = desc("defunct_processes", "Zombie (defunct) processes in the PID namespace, by name and parent name. Zombies under a parent that is not an init (tini, dumb-init) mean nobody reaps exited Chrome children.", "comm", "parent")
	dProfileDirs    = desc("profile_dirs", "Chrome user-data directories by state: active (a running browser uses it), leaked (unused and older than the grace period), pending (unused, still inside the grace period).", "state")
	dProfileBytes   = desc("profile_dir_bytes", "Bytes on disk of Chrome user-data directories by state.", "state")
	dLocks          = desc("profile_locks", "SingletonLock files by state: live, stale (the PID is not a running browser on this host; the next Chrome using this directory may refuse to start) or foreign (another hostname).", "state")
	dEndpoints      = desc("cdp_endpoints", "CDP endpoints by source (static, discovered) and state (up, down).", "source", "state")
	dCDPUp          = desc("cdp_up", "Whether a statically configured CDP endpoint answered /json/version.", "endpoint")
	dTargets        = desc("targets", "CDP targets from /json/list by type (page, iframe, service_worker, worker, other).", "type")
	dContexts       = desc("browser_contexts", "Distinct browser contexts that own at least one target, from watched endpoints. Playwright and browserless create one context per session.")
	dWatched        = desc("cdp_watched_endpoints", "Endpoints with a connected event websocket.")
	dNoDebugger     = desc("browsers_without_debug_endpoint", "Browsers with no discoverable CDP port (for example Playwright's --remote-debugging-pipe). Only /proc metrics cover them.")
	dScrapeDuration = desc("exporter_scrape_duration_seconds", "Time spent in each part of the last scrape.", "collector")
	dUp             = desc("exporter_proc_up", "Whether the last /proc scan succeeded.")
)

func desc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(ns, "", name), help, labels, nil)
}

// New builds a Collector. ctx bounds the lifetime of background watchers.
func New(ctx context.Context, cfg Config) *Collector {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.DiscoverHost == "" {
		cfg.DiscoverHost = "127.0.0.1"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	return &Collector{
		cfg:      cfg,
		ctx:      ctx,
		watchers: map[string]*watch{},
		created: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "targets_created_total",
			Help: "CDP targets created on watched endpoints, by type."}, []string{"type"}),
		destroyed: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "targets_destroyed_total",
			Help: "CDP targets destroyed on watched endpoints, by type."}, []string{"type"}),
		crashes: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "target_crashes_total",
			Help: "Target.targetCrashed events (renderer crashed, was killed or ran out of memory) on watched endpoints."}, []string{"type", "status"}),
		reconnects: prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "cdp_watch_reconnects_total",
			Help: "Times an event websocket was re-established after it dropped while its browser kept running."}),
		rtt: prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "cdp_request_duration_seconds",
			Help:    "Round trip of /json/version probes. A busy or wedged browser answers slowly long before it fails.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "exporter_scrape_errors_total",
			Help: "Errors per collector part."}, []string{"collector"}),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{dBrowsers, dProcs, dRSS, dPSS, dPSSUnreadable, dBrowserMax, dBrowserOldest,
		dOrphans, dOrphanRSS, dDefunct, dProfileDirs, dProfileBytes, dLocks, dEndpoints, dCDPUp, dTargets, dContexts,
		dWatched, dNoDebugger, dScrapeDuration, dUp} {
		ch <- d
	}
	c.created.Describe(ch)
	c.destroyed.Describe(ch)
	c.crashes.Describe(ch)
	c.reconnects.Describe(ch)
	c.rtt.Describe(ch)
	c.scrapeErrors.Describe(ch)
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	gauge := func(d *prometheus.Desc, v float64, l ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, l...)
	}

	t0 := time.Now()
	snap, err := c.cfg.Proc.Scan()
	gauge(dScrapeDuration, time.Since(t0).Seconds(), "proc")
	if err != nil {
		c.cfg.Log.Warn("proc scan failed", "err", err)
		c.scrapeErrors.WithLabelValues("proc").Inc()
		gauge(dUp, 0)
	} else {
		gauge(dUp, 1)
		c.collectProc(snap, gauge)
	}

	if c.cfg.Profiles != nil && snap != nil {
		t1 := time.Now()
		c.collectProfiles(snap, gauge)
		gauge(dScrapeDuration, time.Since(t1).Seconds(), "profiles")
	}

	t2 := time.Now()
	c.collectCDP(snap, gauge)
	gauge(dScrapeDuration, time.Since(t2).Seconds(), "cdp")

	c.created.Collect(ch)
	c.destroyed.Collect(ch)
	c.crashes.Collect(ch)
	c.reconnects.Collect(ch)
	c.rtt.Collect(ch)
	c.scrapeErrors.Collect(ch)
}

func (c *Collector) collectProc(snap *procscan.Snapshot, gauge func(*prometheus.Desc, float64, ...string)) {
	count := map[string]float64{}
	rss := map[string]float64{}
	pss := map[string]float64{}
	unreadable := 0
	type defKey struct{ comm, parent string }
	defunct := map[defKey]float64{}
	for _, p := range snap.Procs {
		if p.State == 'Z' {
			parent := "unknown"
			if pp, ok := snap.Procs[p.PPID]; ok {
				parent = pp.Comm
			}
			defunct[defKey{p.Comm, parent}]++
			continue
		}
		if !p.Chrome {
			continue
		}
		count[p.Type]++
		rss[p.Type] += float64(p.RSS)
		if p.HasPSS {
			pss[p.Type] += float64(p.PSS)
		} else {
			unreadable++
		}
	}
	gauge(dBrowsers, float64(len(snap.Browsers)))
	for t, n := range count {
		gauge(dProcs, n, t)
		gauge(dRSS, rss[t], t)
		if v, ok := pss[t]; ok {
			gauge(dPSS, v, t)
		}
	}
	gauge(dPSSUnreadable, float64(unreadable))
	for k, n := range defunct {
		gauge(dDefunct, n, k.comm, k.parent)
	}

	var maxMem, oldest float64
	for _, b := range snap.Browsers {
		var mem float64
		for _, p := range snap.Tree(b.PID) {
			if !p.Chrome || p.State == 'Z' {
				continue
			}
			if p.HasPSS {
				mem += float64(p.PSS)
			} else {
				mem += float64(p.RSS)
			}
		}
		maxMem = max(maxMem, mem)
		oldest = max(oldest, snap.Taken.Sub(b.Start).Seconds())
	}
	gauge(dBrowserMax, maxMem)
	gauge(dBrowserOldest, oldest)

	orphans := map[string]float64{}
	var orphanRSS float64
	for _, p := range snap.Orphans() {
		orphans[p.Type]++
		orphanRSS += float64(p.RSS)
	}
	for _, t := range []string{"renderer", "zygote", "gpu-process", "utility"} {
		if _, ok := orphans[t]; !ok {
			orphans[t] = 0
		}
	}
	for t, n := range orphans {
		gauge(dOrphans, n, t)
	}
	gauge(dOrphanRSS, orphanRSS)
}

func (c *Collector) collectProfiles(snap *procscan.Snapshot, gauge func(*prometheus.Desc, float64, ...string)) {
	active := map[string]int{}
	for _, b := range snap.Browsers {
		if b.UserDataDir != "" {
			active[b.UserDataDir] = b.PID
		}
	}
	isBrowser := func(pid int) bool {
		p, ok := snap.Procs[pid]
		return ok && p.Chrome && p.Type == procscan.TypeBrowser && p.State != 'Z'
	}
	dirs := c.cfg.Profiles.Scan(active, isBrowser, time.Now())
	n := map[string]float64{"active": 0, "leaked": 0, "pending": 0}
	b := map[string]float64{"active": 0, "leaked": 0, "pending": 0}
	locks := map[string]float64{"live": 0, "stale": 0, "foreign": 0}
	for _, d := range dirs {
		state := "pending"
		switch {
		case d.Active:
			state = "active"
		case d.Leaked:
			state = "leaked"
		}
		n[state]++
		b[state] += float64(d.Bytes)
		if d.Lock != profiles.LockNone {
			locks[string(d.Lock)]++
		}
	}
	for s := range n {
		gauge(dProfileDirs, n[s], s)
		gauge(dProfileBytes, b[s], s)
	}
	for s, v := range locks {
		gauge(dLocks, v, s)
	}
}

type endpoint struct {
	base   string
	source string
}

func (c *Collector) endpoints(snap *procscan.Snapshot) ([]endpoint, int) {
	var eps []endpoint
	seen := map[string]bool{}
	for _, s := range c.cfg.Static {
		s = strings.TrimRight(s, "/")
		if !seen[s] {
			seen[s] = true
			eps = append(eps, endpoint{s, "static"})
		}
	}
	noDebugger := 0
	if c.cfg.Discover && snap != nil {
		for _, b := range snap.Browsers {
			port := b.DebugPort
			if port == 0 && b.UserDataDir != "" {
				port = readActivePort(b.UserDataDir)
			}
			if port <= 0 {
				noDebugger++
				continue
			}
			base := "http://" + net.JoinHostPort(c.cfg.DiscoverHost, strconv.Itoa(port))
			if !seen[base] {
				seen[base] = true
				eps = append(eps, endpoint{base, "discovered"})
			}
		}
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i].base < eps[j].base })
	return eps, noDebugger
}

// readActivePort reads the port Chrome wrote to <user-data-dir>/DevToolsActivePort
// when it was started with --remote-debugging-port=0 (Puppeteer's default).
func readActivePort(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(b), "\n")
	n, _ := strconv.Atoi(strings.TrimSpace(line))
	return n
}

type probeResult struct {
	ep        endpoint
	up        bool
	browserID string
	targets   []cdp.Target
}

// probe calls /json/version (and /json/list when withTargets) on every known
// endpoint and drops duplicates of the same browser.
func (c *Collector) probe(snap *procscan.Snapshot, withTargets bool) ([]probeResult, int) {
	eps, noDebugger := c.endpoints(snap)
	results := make([]probeResult, len(eps))
	var wg sync.WaitGroup
	for i, ep := range eps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c.ctx, c.cfg.Timeout)
			defer cancel()
			r := probeResult{ep: ep}
			if v, d, err := c.cfg.CDP.Version(ctx, ep.base); err == nil {
				if withTargets {
					c.rtt.Observe(d.Seconds())
				}
				r.up = true
				r.browserID = path.Base(v.WebSocketDebuggerURL)
				if withTargets {
					if ts, err := c.cfg.CDP.Targets(ctx, ep.base); err == nil {
						r.targets = ts
					} else {
						c.scrapeErrors.WithLabelValues("cdp").Inc()
					}
				}
			}
			results[i] = r
		}()
	}
	wg.Wait()

	// One browser can be reachable on two endpoints, for example a static
	// socat or Service port and the port discovered from its flags. Count each
	// browser once (by the id in its browser websocket URL), keeping the static
	// endpoint, so targets and watchers are not doubled.
	sort.SliceStable(results, func(i, j int) bool { return results[i].ep.source == "static" && results[j].ep.source != "static" })
	seenBrowser := map[string]bool{}
	kept := results[:0]
	for _, r := range results {
		if r.up && r.browserID != "" && r.browserID != "." {
			if seenBrowser[r.browserID] {
				continue
			}
			seenBrowser[r.browserID] = true
		}
		kept = append(kept, r)
	}
	return kept, noDebugger
}

// Run keeps event watchers in step with the running browsers between scrapes,
// so a crash right after a browser starts is counted even if no scrape has
// happened yet. It returns when ctx is done.
func (c *Collector) Run(ctx context.Context, every time.Duration) {
	if !c.cfg.Watch {
		return
	}
	light := *c.cfg.Proc
	light.ReadPSS = false
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		snap, _ := light.Scan()
		results, _ := c.probe(snap, false)
		c.reconcileWatchers(results)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Collector) collectCDP(snap *procscan.Snapshot, gauge func(*prometheus.Desc, float64, ...string)) {
	results, noDebugger := c.probe(snap, true)
	gauge(dNoDebugger, float64(noDebugger))

	state := map[[2]string]float64{
		{"static", "up"}: 0, {"static", "down"}: 0, {"discovered", "up"}: 0, {"discovered", "down"}: 0,
	}
	byType := map[string]float64{"page": 0}
	for _, r := range results {
		s := "down"
		if r.up {
			s = "up"
		}
		state[[2]string{r.ep.source, s}]++
		if r.ep.source == "static" {
			gauge(dCDPUp, boolf(r.up), r.ep.base)
		}
		for _, t := range r.targets {
			byType[t.Type]++
		}
	}
	for k, v := range state {
		gauge(dEndpoints, v, k[0], k[1])
	}
	for t, v := range byType {
		gauge(dTargets, v, t)
	}

	if !c.cfg.Watch {
		return
	}
	c.reconcileWatchers(results)
	contexts, watched := 0, 0
	c.watchMu.Lock()
	for _, w := range c.watchers {
		st := w.w.State()
		if st.Connected {
			watched++
			contexts += st.Contexts
		}
	}
	c.watchMu.Unlock()
	gauge(dContexts, float64(contexts))
	gauge(dWatched, float64(watched))
}

// reconcileWatchers starts a watcher for every endpoint that is up and stops
// watchers for discovered endpoints whose browser has gone. Static endpoints
// keep their watcher (it reconnects by itself) so a restart is not missed.
func (c *Collector) reconcileWatchers(results []probeResult) {
	want := map[string]string{}
	live := map[string]string{}
	for _, r := range results {
		if r.ep.source == "static" {
			want[r.ep.base] = r.ep.source
		}
		if r.up {
			want[r.ep.base] = r.ep.source
			live[r.ep.base] = r.ep.source
		}
	}
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	for base, w := range c.watchers {
		if _, ok := want[base]; !ok {
			w.cancel()
			delete(c.watchers, base)
		}
	}
	for base, src := range want {
		if _, ok := c.watchers[base]; ok {
			continue
		}
		if _, up := live[base]; !up {
			continue // start once it answers
		}
		ctx, cancel := context.WithCancel(c.ctx)
		w := &cdp.Watcher{Base: base, Client: c.cfg.CDP, Events: c, Log: c.cfg.Log}
		c.watchers[base] = &watch{w: w, cancel: cancel, source: src}
		go w.Run(ctx)
	}
}

// TargetCreated implements cdp.Events.
func (c *Collector) TargetCreated(t string) { c.created.WithLabelValues(normType(t)).Inc() }

// TargetDestroyed implements cdp.Events.
func (c *Collector) TargetDestroyed(t string) { c.destroyed.WithLabelValues(normType(t)).Inc() }

// TargetCrashed implements cdp.Events.
func (c *Collector) TargetCrashed(t, status string) {
	c.crashes.WithLabelValues(normType(t), normStatus(status)).Inc()
}

// Reconnect implements cdp.Events.
func (c *Collector) Reconnect() { c.reconnects.Inc() }

func normType(t string) string {
	switch t {
	case "page", "iframe", "worker", "shared_worker", "service_worker", "browser", "tab", "webview", "background_page", "other":
		return t
	case "":
		return "unknown"
	}
	return "other"
}

// normStatus bounds the label to Chrome's TerminationStatus names.
func normStatus(s string) string {
	switch s {
	case "abnormal", "killed", "crashed", "oom", "launch failed", "integrity failure", "still running", "normal":
		return strings.ReplaceAll(s, " ", "_")
	}
	return "other"
}

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
