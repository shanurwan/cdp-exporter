#!/usr/bin/env bash
# Runs deploy/kubernetes/browserless.yaml on a kind cluster and checks the
# sidecar: metrics through the API server, the readiness gate taking a full
# pod out of the Service, and a SIGKILLed browser.
#
#   ./test/kind/run.sh          # needs docker, kind and kubectl
#   KEEP=1 ./test/kind/run.sh   # leave the cluster running
set -euo pipefail
cd "$(dirname "$0")/../.."

CLUSTER=${CLUSTER:-cdpx-kind}
BL=${BROWSERLESS_IMAGE:-ghcr.io/browserless/chromium:v2.63.1}
NODE=${NODE_IMAGE:-node:24-alpine}
fail=0
results=()
pass() { results+=("PASS  $1"); }
failed() { results+=("FAIL  $1"); fail=1; }

if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 120s
fi
[ "${KEEP:-0}" = 1 ] || trap 'kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true' EXIT
kubectl config use-context "kind-$CLUSTER" >/dev/null

echo "== images"
# Only the exporter is built here; the node pulls browserless and node itself.
docker build -q -t cdp-exporter:kind . >/dev/null
docker save cdp-exporter:kind -o /tmp/cdp-exporter-kind.tar
kind load image-archive --name "$CLUSTER" /tmp/cdp-exporter-kind.tar

echo "== deploy"
kubectl delete pod load --ignore-not-found --now >/dev/null
kubectl create configmap load --from-file=test/soak/load.mjs --dry-run=client -o yaml | kubectl apply -f - >/dev/null
sed "s#image: ghcr.io/shanurwan/cdp-exporter:.*#image: cdp-exporter:kind#; s#image: ghcr.io/browserless/chromium:.*#image: $BL#" \
  deploy/kubernetes/browserless.yaml | kubectl apply -f -
kubectl rollout status deploy/browserless --timeout=600s

pods=($(kubectl get pod -l app=browserless -o jsonpath='{.items[*].metadata.name}'))
A=${pods[0]} B=${pods[1]}
ipA=$(kubectl get pod "$A" -o jsonpath='{.status.podIP}')
metrics() { kubectl get --raw "/api/v1/namespaces/default/pods/$1:9431/proxy/metrics"; }
value() { metrics "$1" | awk -v m="$2" '$1==m {print $2; f=1} END {if (!f) print "absent"}'; }
ready() { kubectl get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'; }
endpoints() { # ready addresses of the browserless Service
  kubectl get endpointslice -l kubernetes.io/service-name=browserless \
    -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.targetRef.name}{" "}{end}'
}
# wait_until <description> <timeout s> <command...>; records seconds taken
wait_until() {
  local what=$1 t=$2 start=$SECONDS; shift 2
  while ! "$@" >/dev/null 2>&1; do
    if (( SECONDS - start > t )); then failed "$what (timeout ${t}s)"; return 0; fi
    sleep 1
  done
  pass "$what after $((SECONDS - start))s"
}
is() { [ "$(value "$1" "$2")" = "$3" ]; }
ready_is() { [ "$(ready "$1")" = "$2" ]; }
endpoint_count() { [ "$(endpoints | wc -w)" = "$1" ]; }

echo "== checks"
wait_until "metrics through the API server" 60 is "$A" chrome_exporter_proc_up 1
wait_until "both pods in the Service" 60 endpoint_count 2

echo "   4 sessions on $A (its capacity)"
kubectl run load --image="$NODE" --restart=Never --overrides='{"spec":{"volumes":[{"name":"s","configMap":{"name":"load"}}],"containers":[{"name":"load","image":"'"$NODE"'","command":["sh","-c","for i in 1 2 3 4; do node /s/load.mjs ws://'"$ipA"':3000/chromium session overrun & done; wait"],"volumeMounts":[{"name":"s","mountPath":"/s"}]}]}}' >/dev/null
wait_until "$A runs 4 browsers" 90 is "$A" chrome_browsers 4
wait_until "$A not ready at capacity" 30 ready_is "$A" False
wait_until "Service drops $A" 15 endpoint_count 1
if [ "$(endpoints)" = "$B " ]; then pass "Service routes only to $B"; else failed "Service endpoints: $(endpoints)"; fi
v=$(value "$A" chrome_process_pss_unreadable)
if [ "$v" = 0 ]; then pass "PSS readable for every Chrome process"; else failed "chrome_process_pss_unreadable=$v"; fi

victim=$(kubectl exec "$A" -c browserless -- sh -c 'for p in $(pgrep -x chrome); do grep -qa -- --type= /proc/$p/cmdline || { echo $p; break; }; done')
echo "   SIGKILL browser pid $victim in $A"
kubectl exec "$A" -c browserless -- kill -9 "$victim"
wait_until "$A runs 3 browsers" 30 is "$A" chrome_browsers 3
wait_until "$A ready again" 30 ready_is "$A" True
sleep 5
z=$(metrics "$A" | awk '/^chrome_defunct_processes/ {s+=$2} END {print s+0}')
if [ "$z" = 0 ]; then pass "no zombies (the pause container reaps them)"; else failed "$z zombies"; fi
echo "   after the kill:"
metrics "$A" | grep -E '^chrome_(browsers|orphan_processes|profile_(dirs|locks)|target_crashes_total)' | grep -v ' 0$' | sed 's/^/     /'

kubectl delete pod load --now >/dev/null
echo
printf '%s\n' "${results[@]}"
exit $fail
