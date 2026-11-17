#!/usr/bin/env bash
# End-to-end checks against real Chrome in Docker. Each check injects a fault
# and records how long the exporter takes to show it.
#
#   ./test/e2e/run.sh            # build the exporter and run every check
#
# Needs docker. Exits non-zero if any check fails.
set -euo pipefail
cd "$(dirname "$0")/../.."

HEADLESS=${HEADLESS_IMAGE:-chromedp/headless-shell:158.0.8127.37}
PW=${PLAYWRIGHT_IMAGE:-mcr.microsoft.com/playwright:v1.56.1-noble}
NODE=${NODE_IMAGE:-node:24-alpine}
CURL=${CURL_IMAGE:-curlimages/curl:8.17.0}
P=cdpx-e2e
fail=0
results=()

cleanup() { docker rm -f $P-chrome $P-exp $P-launch >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "== build"
docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=0 golang:1.26 go build -buildvcs=false -o bin/cdp-exporter ./cmd/cdp-exporter

metrics() { docker run --rm --network=container:$1 "$CURL" -s localhost:9431/metrics; }
value() { # value <container> <metric-with-labels>
  metrics "$1" | awk -v m="$2" '$1==m {print $2; found=1} END {if (!found) print "absent"}'
}
# wait_for <container> <metric> <awk condition on v> <timeout s>; prints seconds taken
wait_for() {
  local start=$(date +%s.%N) v
  while :; do
    v=$(value "$1" "$2")
    if [ "$v" != absent ] && awk -v v="$v" "BEGIN{exit !($3)}"; then
      awk -v s="$start" -v e="$(date +%s.%N)" 'BEGIN{printf "%.1f", e-s}'
      return 0
    fi
    if awk -v s="$start" -v e="$(date +%s.%N)" -v t="$4" 'BEGIN{exit !(e-s>t)}'; then
      echo "timeout (last $2=$v)"
      return 1
    fi
    sleep 0.5
  done
}
check() { # check <name> <container> <metric> <condition> <timeout>
  local took
  if took=$(wait_for "$2" "$3" "$4" "$5"); then
    results+=("PASS  $1: $3 $4 after ${took}s")
  else
    results+=("FAIL  $1: $took"); fail=1
  fi
}
exporter() { # exporter <target container> [flags...]
  local target=$1; shift
  docker run -d --name $P-exp --pid=container:$target --network=container:$target \
    --volumes-from $target -v "$PWD/bin":/b:ro "$NODE" /b/cdp-exporter "$@" >/dev/null
  for _ in $(seq 40); do metrics "$target" >/dev/null 2>&1 && return 0; sleep 0.25; done
  echo "exporter did not start"; docker logs $P-exp; return 1
}

echo "== 1. sessions and a renderer crash (chromedp/headless-shell)"
docker run -d --name $P-chrome --shm-size=1g "$HEADLESS" >/dev/null
sleep 2
exporter $P-chrome --cdp-endpoint=http://127.0.0.1:9222
check "browser seen" $P-chrome chrome_browsers 'v==1' 20
docker run --rm --network=container:$P-chrome -v "$PWD/test/e2e":/e:ro "$NODE" node /e/cdp.mjs http://127.0.0.1:9222 open 5
check "5 isolated sessions" $P-chrome chrome_browser_contexts 'v>=5' 15
check "renderers per session" $P-chrome 'chrome_processes{type="renderer"}' 'v>=5' 15
docker run --rm --network=container:$P-chrome -v "$PWD/test/e2e":/e:ro "$NODE" node /e/cdp.mjs http://127.0.0.1:9222 crash >/dev/null
check "renderer crash counted" $P-chrome 'chrome_target_crashes_total{status="crashed",type="page"}' 'v>=1' 15
docker rm -f $P-exp $P-chrome >/dev/null

for init in no yes; do
  name="SIGKILLed browser, Node as PID 1, init=$init"
  echo "== 2. $name"
  initflag=(); [ $init = yes ] && initflag=(--init)
  docker run -d --name $P-launch "${initflag[@]}" --hostname e2e-worker --shm-size=1g -v /tmp "$PW" node -e "$(cat test/e2e/launcher.mjs)" 3 >/dev/null
  exporter $P-launch --profile-grace=5s --lock-hostname=e2e-worker
  check "$name: 3 browsers" $P-launch chrome_browsers 'v==3' 30
  check "$name: ports discovered" $P-launch 'chrome_cdp_endpoints{source="discovered",state="up"}' 'v==3' 30
  # The first browser process: argv[0] is chrome and there is no --type=.
  victim=$(docker exec $P-launch sh -c 'for p in /proc/[0-9]*; do a0=$(tr "\0" "\n" < $p/cmdline 2>/dev/null | head -1); case "$a0" in */chrome) grep -qa -- "--type=" $p/cmdline || { echo ${p#/proc/}; break; };; esac; done')
  [ -n "$victim" ] || { echo "no browser pid found"; exit 1; }
  echo "   SIGKILL browser pid $victim"
  docker exec $P-launch kill -9 "$victim"
  check "$name: browser gone" $P-launch chrome_browsers 'v==2' 20
  check "$name: stale SingletonLock" $P-launch 'chrome_profile_locks{state="stale"}' 'v==1' 30
  check "$name: leaked profile" $P-launch 'chrome_profile_dirs{state="leaked"}' 'v==1' 30
  if [ $init = no ]; then
    check "$name: zombies under node" $P-launch 'chrome_defunct_processes{comm="chrome",parent="node"}' 'v>=1' 30
  else
    sleep 3
    z=$(metrics $P-launch | awk '/^chrome_defunct_processes/ {s+=$2} END {print s+0}')
    if [ "$z" = 0 ]; then results+=("PASS  $name: no zombies (init reaps them)"); else results+=("FAIL  $name: $z zombies with --init"); fail=1; fi
  fi
  metrics $P-launch | grep -E '^chrome_(browsers|defunct|orphan_processes|profile_(dirs|locks|dir_bytes))' | grep -v ' 0$' | sed 's/^/    /'
  docker rm -f $P-exp $P-launch >/dev/null
done

echo
printf '%s\n' "${results[@]}"
exit $fail
