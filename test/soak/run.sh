#!/usr/bin/env bash
# Soak: browserless under mixed load and faults, with cdp-exporter beside it
# and Prometheus recording every 5 s.
#
#   ./test/soak/run.sh                 # 60 minutes
#   DURATION=600 ./test/soak/run.sh    # 10 minutes
#   WITHOUT=hog ./test/soak/run.sh     # no runaway tab (see load.mjs for kinds)
#
# Output goes to soak-out/: load.log (sessions by kind and result),
# stats.csv (container memory and tasks every 10 s), after.txt (state once the
# load has stopped and the leak grace period has passed) and the Prometheus
# data (volume cdpx-soak-prom). Prometheus keeps running on the browserless network namespace so the
# results can be queried; KEEP=0 removes everything at the end.
set -euo pipefail
cd "$(dirname "$0")/../.."

BL=${BROWSERLESS_IMAGE:-ghcr.io/browserless/chromium:v2.63.1}
PROM=${PROMETHEUS_IMAGE:-prom/prometheus:v3.7.3}
NODE=${NODE_IMAGE:-node:24-alpine}
CURL=${CURL_IMAGE:-curlimages/curl:8.17.0}
DURATION=${DURATION:-3600}
WORKERS=${WORKERS:-10}
MEMORY=${MEMORY:-6g}
OUT=$PWD/soak-out
P=cdpx-soak

docker rm -f $P-bl $P-exp $P-prom $P-load >/dev/null 2>&1 || true
docker volume rm $P-prom >/dev/null 2>&1 || true
rm -rf "$OUT"; mkdir -p "$OUT"

echo "== build image"
docker build -q -t cdp-exporter:soak . >/dev/null

echo "== browserless: 8 concurrent sessions, 60 s session timeout, memory limit $MEMORY"
docker run -d --name $P-bl --hostname soak-bl -v /tmp --shm-size=2g --memory=$MEMORY --memory-swap=$MEMORY \
  -e CONCURRENT=8 -e QUEUED=20 -e TIMEOUT=60000 "$BL" >/dev/null
for _ in $(seq 60); do
  docker run --rm --network=container:$P-bl "$CURL" -sf localhost:3000/json/version >/dev/null 2>&1 && break; sleep 1
done

# Same UID and GID as Chrome (blessuser): the kernel lets a process read
# another's smaps_rollup only when both match.
uid=$(docker exec $P-bl id -u):$(docker exec $P-bl id -g)
docker run -d --name $P-exp --user "$uid" --pid=container:$P-bl --network=container:$P-bl \
  --volumes-from $P-bl cdp-exporter:soak >/dev/null
docker run -d --name $P-prom --network=container:$P-bl \
  -v "$PWD/test/soak/prometheus.yml":/etc/prometheus/prometheus.yml:ro \
  -v "$PWD/deploy/prometheus/alerts.yml":/etc/prometheus/alerts.yml:ro -v $P-prom:/prometheus \
  "$PROM" --config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus >/dev/null

cgdir=/sys/fs/cgroup$(cut -d: -f3 /proc/"$(docker inspect -f '{{.State.Pid}}' $P-bl)"/cgroup)
[ -r "$cgdir/memory.current" ] || cgdir=""
echo "time,mem_bytes,tasks" >"$OUT/stats.csv"
( while [ "$(docker inspect -f '{{.State.Running}}' $P-bl 2>/dev/null)" = true ]; do
    if [ -n "$cgdir" ]; then
      echo "$(date +%s),$(cat $cgdir/memory.current),$(cat $cgdir/pids.current)" >>"$OUT/stats.csv"
    fi
    sleep 10
  done ) &
sampler=$!
trap 'kill $sampler 2>/dev/null || true' EXIT

echo "== load: $WORKERS clients for ${DURATION}s${WITHOUT:+, without $WITHOUT}"
docker run --name $P-load --network=container:$P-bl -e WITHOUT="${WITHOUT:-}" -v "$PWD/test/soak":/s:ro "$NODE" \
  node /s/load.mjs ws://127.0.0.1:3000/chromium "$WORKERS" "$DURATION" >"$OUT/load.log" 2>&1 || true
docker rm -f $P-load >/dev/null
tail -1 "$OUT/load.log"

echo "== load stopped; waiting ${SETTLE:-150} s (profile grace is 2m)"
sleep "${SETTLE:-150}"
{
  echo "# $(date -u +%FT%TZ) metrics after the load stopped"
  docker run --rm --network=container:$P-bl "$CURL" -s localhost:9431/metrics | grep -E '^chrome_' | grep -vE '_bucket|exporter_build'
  echo "# processes in the browserless container"
  docker exec $P-bl ps -eo pid,ppid,stat,etimes,rss,comm || docker inspect -f 'browserless exited: OOMKilled={{.State.OOMKilled}} code={{.State.ExitCode}} at {{.State.FinishedAt}}' $P-bl
  echo "# profile directories"
  docker exec $P-bl sh -c 'ls -la /tmp/browserless-data-dirs/ 2>&1' || true
  [ -n "$cgdir" ] && { echo "# memory.events"; cat $cgdir/memory.events; }
} >"$OUT/after.txt" 2>&1
kill $sampler 2>/dev/null || true
echo "== done: $OUT"

if [ "${KEEP:-1}" = 0 ]; then docker rm -f $P-bl $P-exp $P-prom >/dev/null; fi
