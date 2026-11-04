#!/usr/bin/env bash
# Copies the Fluent Bit capture off every kind node and splits it into one
# file per pod container. The capture is a stream of JSON records whose
# "file" key is the CRI log file name, <pod>_<namespace>_<container>-<id>.log,
# so the split needs nothing but that name.
#
# Then writes redis-diagnostics.txt: the pods and events of the counter store
# and the report of each of its pods that is still running. A pod replaced
# during the run wrote the same report to its log before it stopped.
set -euo pipefail

OUT_DIR="${OUT_DIR:-pod-logs}"
KIND_CLUSTER="${KIND_CLUSTER:-ratelimit-e2e}"
REDIS_NAMESPACE="${E2E_REDIS_NAMESPACE:-ratelimit-e2e-redis}"

# Runs one command of the store report. The store may be gone at this point of
# the job, so a failure is written into the report rather than failing the step,
# and a hung exec ends here rather than at the timeout of the job.
report() {
  echo "--- $*"
  timeout 60 "$@" || echo "Exit code $? (124 means the 60-second timeout ran out)"
}

report_store() {
  report kubectl get pods -n "${REDIS_NAMESPACE}" -o wide
  report kubectl describe pods -n "${REDIS_NAMESPACE}"
  report kubectl get events -n "${REDIS_NAMESPACE}" --sort-by=.lastTimestamp

  local pods
  if ! pods=$(timeout 60 kubectl get pods -n "${REDIS_NAMESPACE}" -l app=e2e-redis -o name); then
    echo "Could not list the app=e2e-redis pods in ${REDIS_NAMESPACE}; the errors above give the reason."
  elif [[ -z "${pods}" ]]; then
    echo "Found no app=e2e-redis pod in ${REDIS_NAMESPACE} at the end of the run."
  fi
  if [[ -z "${pods}" ]]; then
    echo "The Redis log files next to this one hold the report of each pod that was stopped."
    return 0
  fi

  for pod in ${pods}; do
    report kubectl exec -n "${REDIS_NAMESPACE}" "${pod}" -c redis -- sh /diagnostics/dump.sh
  done
}

mkdir -p "${OUT_DIR}"

nodes=$(docker ps --format '{{.Names}}' | grep "^${KIND_CLUSTER}-" || true)
if [[ -z "${nodes}" ]]; then
  echo "No kind nodes found for cluster ${KIND_CLUSTER}" >&2
  exit 1
fi

for node in ${nodes}; do
  if ! docker exec "${node}" sh -c 'test -f /var/log/fluent-bit/e2e-pods.log'; then
    echo "No capture on ${node}"
    continue
  fi

  capture=$(mktemp)
  docker exec "${node}" cat /var/log/fluent-bit/e2e-pods.log > "${capture}"

  jq -Rr 'fromjson? | .file // empty' "${capture}" | sort -u | while read -r file; do
    name=$(basename "${file}" .log)
    # Cut the trailing container id; what remains is pod_namespace_container.
    name="${name%-*}"
    # An empty line of a log has no "log" key.
    jq -Rr --arg f "${file}" 'fromjson? | select(.file == $f) | .log // ""' \
      "${capture}" >> "${OUT_DIR}/${name}.log"
    echo "  ${OUT_DIR}/${name}.log"
  done

  rm -f "${capture}"
done

report_store > "${OUT_DIR}/redis-diagnostics.txt" 2>&1
echo "  ${OUT_DIR}/redis-diagnostics.txt"
