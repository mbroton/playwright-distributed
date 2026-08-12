#!/usr/bin/env bash

# Checks that the selector routes only to workers whose Redis key still has a
# TTL. A registration that keeps its fields but loses its expiry must not
# receive sessions, because nothing else proves the worker is alive.

set -euo pipefail

compose=(docker compose -f docker-compose.local.yaml)
if [[ -n "${COMPOSE_OVERRIDE_FILE:-}" ]]; then
    compose+=(-f "$COMPOSE_OVERRIDE_FILE")
fi

redis=("${compose[@]}" exec -T redis redis-cli --raw)

worker_id=chromium:selector-test
worker_key="worker:$worker_id"
selector=$(<proxy/internal/redis/selector.lua)

cleanup() {
    "${redis[@]}" del "$worker_key" >/dev/null
    "${redis[@]}" hdel cluster:active_connections "$worker_id" >/dev/null
    "${redis[@]}" hdel cluster:lifetime_connections "$worker_id" >/dev/null
}
trap cleanup EXIT

# The stale lastHeartbeat value also proves that liveness no longer depends on
# that field. A higher lifetime count than the real workers makes the selector
# prefer this worker.
register_test_worker() {
    "${redis[@]}" hset "$worker_key" \
        status available browserType chromium lastHeartbeat 0 >/dev/null
    "${redis[@]}" hset cluster:active_connections "$worker_id" 0 >/dev/null
    "${redis[@]}" hset cluster:lifetime_connections "$worker_id" 1 >/dev/null
}

select_worker() {
    "${redis[@]}" eval "$selector" 0 5 100 chromium
}

register_test_worker
"${redis[@]}" expire "$worker_key" 60 >/dev/null

selected=$(select_worker)
if [[ "$selected" != "$worker_id" ]]; then
    echo "Expected $worker_id, selected ${selected:-none}" >&2
    exit 1
fi

# A selection increments the connection counters, so reset them before the
# second run.
register_test_worker
"${redis[@]}" persist "$worker_key" >/dev/null

selected_without_ttl=$(select_worker)
if [[ "$selected_without_ttl" == "$worker_id" ]]; then
    echo "Selected persistent worker key $worker_id" >&2
    exit 1
fi

echo 'Selector worker liveness test passed.'
