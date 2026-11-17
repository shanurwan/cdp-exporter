// Command cdp-exporter exports Prometheus metrics for headless Chrome fleets:
// what each browser costs, what it leaks, and whether it still answers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/shanurwan/cdp-exporter/internal/cdp"
	"github.com/shanurwan/cdp-exporter/internal/collector"
	"github.com/shanurwan/cdp-exporter/internal/procscan"
	"github.com/shanurwan/cdp-exporter/internal/profiles"
)

var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cdp-exporter:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen       = flag.String("listen", ":9431", "address for /metrics, /healthz and /readyz")
		procRoot     = flag.String("proc-root", "/proc", "procfs mount to scan (the browser's PID namespace must be visible)")
		namePattern  = flag.String("process-pattern", procscan.DefaultNamePattern.String(), "regexp matched against a process's executable name or comm")
		pss          = flag.Bool("pss", true, "read PSS from smaps_rollup (more accurate for Chrome, costs more)")
		discover     = flag.Bool("discover", true, "find each browser's CDP port from its flags or DevToolsActivePort file")
		discoverHost = flag.String("discover-host", "127.0.0.1", "host used to reach discovered CDP ports")
		watch        = flag.Bool("watch", true, "keep an event websocket open per reachable endpoint (crashes, contexts)")
		timeout      = flag.Duration("cdp-timeout", 3*time.Second, "timeout of each CDP HTTP call")
		profileGrace = flag.Duration("profile-grace", 2*time.Minute, "age after which an unused profile directory counts as leaked")
		sizeTTL      = flag.Duration("profile-size-ttl", 30*time.Second, "how long a profile directory size is cached")
		noProfiles   = flag.Bool("no-profiles", false, "disable profile directory scanning")
		lockHost     = flag.String("lock-hostname", "", "hostname Chrome writes into SingletonLock (default: this host's name)")
		maxBrowsers  = flag.Int("ready-max-browsers", 0, "report not ready when this many browsers run (0 = no limit)")
		maxMemory    = flag.Int64("ready-max-browser-memory-bytes", 0, "report not ready when the largest browser uses more than this (0 = no limit)")
		logLevel     = flag.String("log-level", "info", "debug, info, warn or error")
		showVersion  = flag.Bool("version", false, "print the version and exit")
		static       multiFlag
		globs        multiFlag
	)
	flag.Var(&static, "cdp-endpoint", "CDP HTTP endpoint to probe, e.g. http://127.0.0.1:9222 (repeatable)")
	flag.Var(&globs, "profile-glob", "glob of temporary profile directories (repeatable; default: Puppeteer and Playwright temp dirs)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	re, err := regexp.Compile(*namePattern)
	if err != nil {
		return fmt.Errorf("-process-pattern: %w", err)
	}
	ps := procscan.New(*procRoot)
	ps.NamePattern = re
	ps.ReadPSS = *pss
	if _, err := ps.Scan(); err != nil {
		return fmt.Errorf("cannot scan %s: %w", *procRoot, err)
	}

	var prof *profiles.Scanner
	if !*noProfiles {
		if len(globs) == 0 {
			globs = profiles.DefaultGlobs
		}
		host := *lockHost
		if host == "" {
			host, _ = os.Hostname()
		}
		prof = &profiles.Scanner{Globs: globs, Grace: *profileGrace, Hostname: host, SizeTTL: *sizeTTL}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := cdp.NewClient(*timeout)
	col := collector.New(ctx, collector.Config{
		Proc: ps, Profiles: prof, CDP: client, Static: static,
		Discover: *discover, DiscoverHost: *discoverHost, Watch: *watch, Timeout: *timeout, Log: log,
	})
	reg := prometheus.NewRegistry()
	reg.MustRegister(col,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)

	readyScan := procscan.New(*procRoot)
	readyScan.NamePattern = re
	readyScan.ReadPSS = *pss && *maxMemory > 0

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if reason := notReady(r.Context(), client, static, readyScan, *maxBrowsers, *maxMemory); reason != "" {
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "cdp-exporter %s\n/metrics /healthz /readyz\n", version)
	})

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go col.Run(ctx, 5*time.Second)
	log.Info("listening", "addr", *listen, "version", version, "static_endpoints", static.String(), "discover", *discover)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// notReady returns why the browser container should not take new sessions, or
// "" when it may. Every static endpoint must answer, and the optional capacity
// limits must not be reached. A Kubernetes Service then stops routing new
// connections to a full pod without killing the sessions it already holds.
func notReady(ctx context.Context, c *cdp.Client, static []string, ps *procscan.Scanner, maxBrowsers int, maxMem int64) string {
	for _, ep := range static {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, _, err := c.Version(cctx, ep)
		cancel()
		if err != nil {
			return fmt.Sprintf("cdp endpoint %s: %v", ep, err)
		}
	}
	if maxBrowsers <= 0 && maxMem <= 0 {
		return ""
	}
	snap, err := ps.Scan()
	if err != nil {
		return "proc scan: " + err.Error()
	}
	if maxBrowsers > 0 && len(snap.Browsers) >= maxBrowsers {
		return fmt.Sprintf("at capacity: %d browsers (limit %d)", len(snap.Browsers), maxBrowsers)
	}
	if maxMem > 0 {
		for _, b := range snap.Browsers {
			var mem int64
			for _, p := range snap.Tree(b.PID) {
				if !p.Chrome || p.State == 'Z' {
					continue
				}
				if p.HasPSS {
					mem += int64(p.PSS)
				} else {
					mem += int64(p.RSS)
				}
			}
			if mem > maxMem {
				return fmt.Sprintf("browser pid %d uses %d bytes (limit %d)", b.PID, mem, maxMem)
			}
		}
	}
	return ""
}

func buildInfo() prometheus.Collector {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "chrome", Name: "exporter_build_info",
		Help: "cdp-exporter version."}, []string{"version"})
	g.WithLabelValues(version).Set(1)
	return g
}
