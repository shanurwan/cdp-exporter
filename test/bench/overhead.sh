#!/usr/bin/env bash
# What the exporter costs: CPU and memory while N browsers with T tabs each
# run and Prometheus-style scrapes arrive every EVERY seconds. Measuring starts
# once the tabs have loaded and the host has settled.
#
#   ./test/bench/overhead.sh            # 30 browsers x 4 tabs, 15 s scrapes, 5 minutes
#   EXPORTER_ARGS=--pss=false ./test/bench/overhead.sh
#   BROWSERS=10 TABS=2 EVERY=5 SECONDS_=120 ./test/bench/overhead.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

PW=${PLAYWRIGHT_IMAGE:-mcr.microsoft.com/playwright:v1.56.1-noble}
NODE=${NODE_IMAGE:-node:24-alpine}
CURL=${CURL_IMAGE:-curlimages/curl:8.17.0}
BROWSERS=${BROWSERS:-30}
TABS=${TABS:-4}
EVERY=${EVERY:-15}
RUN=${SECONDS_:-300}
SETTLE=${SETTLE:-60}
P=cdpx-bench
cleanup() { docker rm -f $P-chrome $P-exp >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=0 golang:1.26 go build -buildvcs=false -o bin/cdp-exporter ./cmd/cdp-exporter

echo "== $BROWSERS browsers x $TABS tabs"
docker run -d --name $P-chrome --shm-size=2g -v /tmp -v "$PWD/test/e2e":/e:ro "$PW" node /e/launcher.mjs "$BROWSERS" >/dev/null
for _ in $(seq 60); do
  n=$(docker exec $P-chrome sh -c 'ls /tmp/puppeteer_dev_chrome_profile-*/DevToolsActivePort 2>/dev/null | wc -l')
  [ "$n" = "$BROWSERS" ] && break; sleep 1
done
ports=$(docker exec $P-chrome sh -c 'for f in /tmp/puppeteer_dev_chrome_profile-*/DevToolsActivePort; do head -1 $f; done')
for port in $ports; do
  docker run --rm --network=container:$P-chrome -v "$PWD/test/e2e":/e:ro "$NODE" node /e/cdp.mjs "http://127.0.0.1:$port" open "$TABS" >/dev/null &
done
wait

docker run -d --name $P-exp --pid=container:$P-chrome --network=container:$P-chrome \
  --volumes-from $P-chrome -v "$PWD/bin":/b:ro "$NODE" /b/cdp-exporter ${EXPORTER_ARGS:-} >/dev/null
echo "== settling ${SETTLE}s"
sleep "$SETTLE"

scrape() { docker run --rm --network=container:$P-chrome "$CURL" -s localhost:9431/metrics; }
get() { awk -v m="$1" '$1==m {print $2}'; }
m0=$(scrape)
procs=$(echo "$m0" | awk '/^chrome_processes\{/ {s+=$2} END {print s}')
cpu0=$(echo "$m0" | get process_cpu_seconds_total)
echo "== scraping every ${EVERY}s for ${RUN}s ($procs Chrome processes)"
# One long-running curl container, so container start-up is not in the loop.
# Each scrape prints its own per-part durations.
durations=$(docker run --rm --network=container:$P-chrome --entrypoint sh "$CURL" -c \
  "end=\$((\$(date +%s)+$RUN)); while [ \$(date +%s) -lt \$end ]; do curl -s localhost:9431/metrics | grep '^chrome_exporter_scrape_duration_seconds'; sleep $EVERY; done")
m1=$(scrape)
cpu1=$(echo "$m1" | get process_cpu_seconds_total)
rss=$(echo "$m1" | get process_resident_memory_bytes)
heap=$(echo "$m1" | get go_memstats_heap_inuse_bytes)
echo
echo "browsers        $(echo "$m1" | get chrome_browsers)"
echo "chrome procs    $procs"
echo "pages           $(echo "$m1" | get 'chrome_targets{type="page"}')"
echo "watched         $(echo "$m1" | get chrome_cdp_watched_endpoints)"
awk -v a="$cpu0" -v b="$cpu1" -v t="$RUN" 'BEGIN {printf "exporter CPU    %.2f%% of one core (%.2f s over %d s)\n", 100*(b-a)/t, b-a, t}'
awk -v r="$rss" -v h="$heap" 'BEGIN {printf "exporter RSS    %.1f MiB (Go heap in use %.1f MiB)\n", r/1048576, h/1048576}'
echo "$durations" | sed -E 's/.*collector="([a-z]+)"\} (.*)/\1 \2/' |
  awk '{n[$1]++; s[$1]+=$2; if ($2>m[$1]) m[$1]=$2}
       END {for (c in n) printf "scrape %-9s mean %6.1f ms  max %6.1f ms  (%d scrapes)\n", c, 1000*s[c]/n[c], 1000*m[c], n[c]}' | sort
