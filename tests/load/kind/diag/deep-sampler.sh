#!/usr/bin/env bash
# Samples every hop of the rate limit path every 5 s until <dir>/stop exists:
# the gateway's Envoy (request time, the call to the RLS, the backend, the
# filter's outcomes), every RLS pod (check and store histograms, store
# errors, GC, CPU), Redis (commandstats, the evalsha latency histogram, the
# slowlog, which it then resets, CPU and clients), and the CPU throttling of
# every container from the kubelet. Each sample is one file per source named
# by the epoch milliseconds. Every 15 s it also records the kind node's CPU.
# On every sample it appends the CPU time of the whole runner VM, the kind
# node and steal included, to host-cpu.log; at the start it writes lscpu's
# description of the runner's CPU to lscpu.txt.
#
#   LOAD_NAMESPACE=... REDIS_NAMESPACE=... deep-sampler.sh <dir>
set -uo pipefail
OUT="$1"
NS="${LOAD_NAMESPACE:-ratelimit-load}"
RNS="${REDIS_NAMESPACE:-ratelimit-load-redis}"
NODE="${KIND_NODE:-ratelimit-load-control-plane}"
mkdir -p "$OUT"/gw "$OUT"/svc "$OUT"/redis "$OUT"/cadv
lscpu >"$OUT/lscpu.txt" 2>&1
pw="$(kubectl get secret e2e-redis-auth -n "$RNS" -o jsonpath='{.data.password}' | base64 -d)"

pods=()
ports=()
pids=()
port=19280
for pod in $(kubectl get pods -n "$NS" -l app.kubernetes.io/name=ratelimit-service -o name); do
  kubectl port-forward -n "$NS" "$pod" "$port:8080" >/dev/null 2>&1 &
  pids+=($!)
  pods+=("${pod#pod/}")
  ports+=("$port")
  port=$((port + 1))
done
# The gateway's stats come from Envoy's Prometheus port over a port-forward.
# A kubectl exec of pilot-agent on every sample throttles the gateway
# container's CPU, and the requests that arrive meanwhile wait in it.
gw_port=19290
kubectl port-forward -n "$NS" deploy/public-gateway-istio "$gw_port:15090" >/dev/null 2>&1 &
pids+=($!)
trap 'kill "${pids[@]}" 2>/dev/null' EXIT
sleep 3

now() { date +%s%3N; }
n=0
while [ ! -e "$OUT/stop" ]; do
  t0=$(now)
  curl -sf -m 3 "http://127.0.0.1:$gw_port/stats/prometheus" \
    | grep -E '^envoy_' | grep -E '9000\|\|ratelimit|loadtest-backend|downstream_rq|_ratelimit_' >"$OUT/gw/$t0" &
  jobs_now=($!)
  for i in "${!ports[@]}"; do
    curl -sf -m 3 "http://127.0.0.1:${ports[$i]}/metrics" \
      | grep -E '^(ratelimit_(check_duration|store_roundtrip|store_errors|store_pool_|checks_total)|go_gc_duration|go_goroutines|process_cpu_seconds)' \
      | sed "s/^/${pods[$i]} /"
  done >"$OUT/svc/$t0" &
  jobs_now+=($!)
  # shellcheck disable=SC2016 # the script runs in the Redis pod, with the password as $1
  kubectl exec -n "$RNS" deploy/e2e-redis -- sh -c '
    p="$1"
    echo "## commandstats"; redis-cli --no-auth-warning -a "$p" INFO commandstats
    echo "## cpu"; redis-cli --no-auth-warning -a "$p" INFO cpu
    echo "## clients"; redis-cli --no-auth-warning -a "$p" INFO clients
    echo "## stats"; redis-cli --no-auth-warning -a "$p" INFO stats
    echo "## histogram"; redis-cli --no-auth-warning -a "$p" LATENCY HISTOGRAM evalsha eval
    echo "## slowlog"; redis-cli --no-auth-warning -a "$p" SLOWLOG GET 1024
    redis-cli --no-auth-warning -a "$p" SLOWLOG RESET >/dev/null' _ "$pw" >"$OUT/redis/$t0" 2>/dev/null &
  jobs_now+=($!)
  kubectl get --raw "/api/v1/nodes/$NODE/proxy/metrics/cadvisor" 2>/dev/null \
    | grep -E '^container_cpu_(cfs_throttled_periods_total|cfs_periods_total|usage_seconds_total|cfs_throttled_seconds_total)\{' \
    | grep -E 'container="(service|istio-proxy|redis|generator|nginx)"' >"$OUT/cadv/$t0" &
  jobs_now+=($!)
  { printf '%s ' "$t0"; head -1 /proc/stat; } >>"$OUT/host-cpu.log"
  if [ $((n % 3)) = 0 ]; then
    { printf '%s ' "$t0"; docker stats --no-stream --format '{{.CPUPerc}} {{.MemUsage}}' "$NODE"; } >>"$OUT/node-cpu.log" &
    jobs_now+=($!)
  fi
  # Only this sample's jobs: a bare wait would also wait for the
  # port-forwards, which never end.
  wait "${jobs_now[@]}"
  n=$((n + 1))
  left=$((5000 - ($(now) - t0)))
  if [ "$left" -gt 0 ]; then
    sleep "$(awk -v l="$left" 'BEGIN { printf "%.3f", l / 1000 }')"
  fi
done
