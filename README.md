# cdp-exporter

Prometheus metrics for fleets of headless Chrome: what each browser costs, what it leaks, and whether it still answers.

A browser container that looks healthy to the orchestrator can be failing in ways only the browser shows. Tabs crash, renderers keep growing, a browser killed by the OOM killer leaves zombie processes, a temporary profile and a `SingletonLock` behind. cAdvisor sees one RSS number for the whole container, and the browser pools people self-host do not export metrics for any of this ([browserless#3892](https://github.com/browserless/browserless/issues/3892), [browserless#1698](https://github.com/browserless/browserless/issues/1698), [steel-browser#367](https://github.com/steel-dev/steel-browser/issues/367)). The underlying problems are long-standing: [puppeteer#1825](https://github.com/puppeteer/puppeteer/issues/1825) (zombies), [puppeteer#5893](https://github.com/puppeteer/puppeteer/issues/5893) (memory), [browserless#5343](https://github.com/browserless/browserless/pull/5343) (leaked profiles), [browserless#5689](https://github.com/browserless/browserless/pull/5689) (stale `SingletonLock` after an OOM).

cdp-exporter runs next to the browsers, as a sidecar or in the same container, and reads two sources:

- **`/proc`**, for every Chrome process whatever started it (Puppeteer, Playwright, browserless, chromedp, a plain `chrome --headless`). Processes are grouped by `--type` and by the browser they descend from. This part needs no CDP port, so it also covers Playwright browsers on `--remote-debugging-pipe`.
- **The DevTools protocol**, on ports given with `--cdp-endpoint` and on ports it discovers from each browser's `--remote-debugging-port` flag or `DevToolsActivePort` file (Puppeteer's `--remote-debugging-port=0`). One websocket per browser listens to `Target` events. It never attaches to a page or changes browser state.

## What it catches

`test/e2e/run.sh` runs each fault against real Chrome in Docker and waits for the metric. These times come from one run on a 12-core Linux host, with metrics read every 0.5 s:

| Fault | Signal | Seen after |
|---|---|---|
| 5 clients open isolated sessions | `chrome_browser_contexts` = 5 | 0.4 s |
| A renderer crashes (`chrome://crash`) | `chrome_target_crashes_total{status="crashed"}` +1 | 0.6 s |
| A browser is SIGKILLed (OOM killer, `docker kill`, a crashed driver) | `chrome_browsers` drops | 0.5 s |
| ... its profile is left on disk | `chrome_profile_dirs{state="leaked"}` = 1 (6.8 MB) | 2.4 s |
| ... its `SingletonLock` names a dead PID | `chrome_profile_locks{state="stale"}` = 1 | 0.5 s |
| ... and Node is PID 1 with no init | `chrome_defunct_processes{comm="chrome",parent="node"}` = 7, plus 2 `chrome_crashpad` | 0.5 s |

With `docker run --init`, the same kill leaves no zombies, but the leaked profile and the stale lock are still there. An init process reaps zombies; it does not clean up after Chrome.

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `chrome_browsers` | | Running browser processes (one per browser instance or session) |
| `chrome_processes` | `type` | Running Chrome processes by `--type`: browser, renderer, zygote, gpu-process, utility |
| `chrome_process_resident_memory_bytes` | `type` | Sum of RSS. Shared pages count once per process, so this overstates |
| `chrome_process_proportional_memory_bytes` | `type` | Sum of PSS from `smaps_rollup`, for processes where it is readable |
| `chrome_process_pss_unreadable` | | Processes left out of the PSS sum |
| `chrome_browser_memory_max_bytes` | | The largest browser together with its descendants |
| `chrome_browser_oldest_age_seconds` | | Age of the oldest browser. A browser that never goes away is usually a leaked session |
| `chrome_orphan_processes` | `type` | Renderers, zygotes and utilities whose browser is gone |
| `chrome_orphan_resident_memory_bytes` | | RSS held by orphans |
| `chrome_defunct_processes` | `comm`, `parent` | Zombies in the PID namespace and who fails to reap them |
| `chrome_profile_dirs` | `state` | User-data directories: `active`, `leaked`, or `pending` (unused, inside the grace period) |
| `chrome_profile_dir_bytes` | `state` | Their size on disk |
| `chrome_profile_locks` | `state` | `SingletonLock` files: `live`, `stale` (dead PID on this host), `foreign` (another hostname) |
| `chrome_cdp_endpoints` | `source`, `state` | Static and discovered endpoints, up or down. Each browser is counted once even when reachable on two ports |
| `chrome_cdp_up` | `endpoint` | Per static endpoint |
| `chrome_cdp_request_duration_seconds` | | Histogram of `/json/version` round trips. A busy browser answers slowly before it fails |
| `chrome_targets` | `type` | Targets from `/json/list`: page, iframe, service_worker, ... |
| `chrome_browser_contexts` | | Browser contexts that own a target. Playwright and browserless use one per client |
| `chrome_targets_created_total`, `chrome_targets_destroyed_total` | `type` | Target churn |
| `chrome_target_crashes_total` | `type`, `status` | `Target.targetCrashed` events: crashed, killed, oom, ... |
| `chrome_browsers_without_debug_endpoint` | | Browsers only `/proc` can see (pipe mode) |
| `chrome_cdp_watched_endpoints`, `chrome_cdp_watch_reconnects_total` | | Event websocket health |
| `chrome_exporter_*` | | Scrape duration per part, errors, build info |

No metric has a PID or session label, so cardinality does not grow with traffic.

## Run it

The exporter must see the browser's PID namespace and, for profile metrics, the same `/tmp`. Run it as the same UID as Chrome: `/proc/<pid>/smaps_rollup` (PSS) and Chrome's `0700` profile subdirectories are readable only by that user. The image runs as UID 65532 by default; add `--user 0` when the browser runs as root, as most browser images do.

Docker, next to an existing browser container:

```sh
docker build -t cdp-exporter .
docker run -d --name cdp-exporter --user 0 \
  --pid=container:chrome --network=container:chrome --volumes-from chrome \
  cdp-exporter --cdp-endpoint=http://127.0.0.1:9222
curl -s localhost:9431/metrics   # from inside the shared network namespace
```

Kubernetes: add it as a sidecar with `shareProcessNamespace: true` in the pod spec and mount the browser's `/tmp` volume (an `emptyDir`) in both containers. `SingletonLock` holds the pod's hostname, which both containers share, so lock states work without flags.

From source:

```sh
go build ./cmd/cdp-exporter
```

## Health and readiness

- `/healthz` answers while the exporter runs.
- `/readyz` fails when a static endpoint does not answer `/json/version`. With `--ready-max-browsers=N` or `--ready-max-browser-memory-bytes=B` it also fails when the pod is full. Used as the readiness probe of a sidecar, this makes the Service stop sending new sessions to that pod. Sessions it already holds keep their connections.

## Flags

| Flag | Default | |
|---|---|---|
| `--listen` | `:9431` | |
| `--cdp-endpoint` | | Repeatable. A DNS name works: the exporter sends `Host: localhost`, which Chrome requires since its DNS-rebinding check |
| `--discover` | `true` | Find ports from browser flags and `DevToolsActivePort` |
| `--discover-host` | `127.0.0.1` | |
| `--watch` | `true` | Event websocket per browser |
| `--proc-root` | `/proc` | |
| `--process-pattern` | Chrome, Chromium, headless-shell names | Regexp on the executable name or kernel `comm` |
| `--pss` | `true` | Read `smaps_rollup`. Needs the same UID as Chrome, or `CAP_SYS_PTRACE` |
| `--profile-glob` | Puppeteer and Playwright temp dirs | Repeatable |
| `--profile-grace` | `2m` | Age before an unused profile counts as leaked |
| `--lock-hostname` | this host | Hostname Chrome writes into `SingletonLock` |
| `--no-profiles` | `false` | |
| `--ready-max-browsers`, `--ready-max-browser-memory-bytes` | `0` (off) | |

## Limits

- RSS sums double-count shared memory. Use the PSS metrics where they are readable.
- When a proxy resolves the target, nothing here sees it: the exporter measures the browser, not the sites it visits.
- A browser on `--remote-debugging-pipe` (Playwright's default) has no port to watch, so its crashes and contexts are not counted; `/proc` metrics still cover it.
- Browser processes in another PID namespace are invisible. Run one exporter per pod or container.

## Tests

```sh
go test -race ./...      # unit tests: fake /proc, profiles, a fake DevTools server
./test/e2e/run.sh        # real Chrome in Docker; prints a PASS/FAIL line per check
```

## License

Apache-2.0
