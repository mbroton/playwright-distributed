#!/usr/bin/env bash

set -euo pipefail

compose=(docker compose -f docker-compose.local.yaml)
if [[ -n "${COMPOSE_OVERRIDE_FILE:-}" ]]; then
    compose+=(-f "$COMPOSE_OVERRIDE_FILE")
fi
ws_endpoint="${WS_ENDPOINT:-ws://127.0.0.1:8080}"

worker_start_time() {
    "${compose[@]}" exec -T worker sh -c "awk '{print \$22}' /proc/1/stat"
}

wait_for_redis() {
    for _ in {1..30}; do
        if "${compose[@]}" exec -T redis redis-cli ping 2>/dev/null | grep -q PONG; then
            return
        fi
        sleep 1
    done

    echo 'Redis did not become ready.' >&2
    return 1
}

run_smoke_test() {
    for _ in {1..15}; do
        if WS_ENDPOINT="$ws_endpoint" npm --prefix worker run smoke; then
            return
        fi
        sleep 2
    done

    echo 'Smoke test did not pass.' >&2
    return 1
}

wait_for_worker_restart() {
    local previous_start_time="$1"
    local current_start_time

    for _ in {1..30}; do
        current_start_time=$(worker_start_time 2>/dev/null || true)
        if [[ -n "$current_start_time" && "$current_start_time" != "$previous_start_time" ]]; then
            return
        fi
        sleep 1
    done

    echo 'Worker did not restart.' >&2
    return 1
}

wait_for_new_worker_registration() {
    local previous_key="$1"
    local worker_keys

    for _ in {1..30}; do
        worker_keys=$("${compose[@]}" exec -T redis redis-cli --raw --scan --pattern 'worker:chromium:*')
        while IFS= read -r worker_key; do
            if [[ -n "$worker_key" && "$worker_key" != "$previous_key" ]]; then
                return
            fi
        done <<< "$worker_keys"
        sleep 1
    done

    echo 'Restarted worker did not register in Redis.' >&2
    return 1
}

initial_start_time="$(worker_start_time)"

"${compose[@]}" stop redis

set +e
startup_output=$("${compose[@]}" run --rm --no-deps \
    -e REDIS_URL=redis://:redis-log-secret@redis:6379 \
    -e REDIS_RETRY_ATTEMPTS=1 \
    worker 2>&1)
startup_status=$?
set -e

if (( startup_status == 0 )); then
    echo 'Worker unexpectedly started without Redis.' >&2
    exit 1
fi

if [[ "$startup_output" == *redis-log-secret* ]]; then
    echo 'Worker exposed Redis credentials in its startup logs.' >&2
    exit 1
fi

"${compose[@]}" start redis
wait_for_redis
run_smoke_test

if [[ "$(worker_start_time)" != "$initial_start_time" ]]; then
    echo 'Worker restarted during a recoverable Redis outage.' >&2
    exit 1
fi

worker_logs=$("${compose[@]}" logs worker)
if [[ "$worker_logs" != *'Redis state restored after reconnect'* ]]; then
    echo 'Worker did not restore its Redis state after reconnecting.' >&2
    exit 1
fi

worker_key=$("${compose[@]}" exec -T redis redis-cli --raw --scan --pattern 'worker:chromium:*' | head -n 1)
if [[ -z "$worker_key" ]]; then
    echo 'No Chromium worker registration found in Redis.' >&2
    exit 1
fi

start_time_before_shutdown="$(worker_start_time)"
command_channel="worker:cmd:${worker_key#worker:}"
"${compose[@]}" exec -T redis redis-cli publish "$command_channel" shutdown >/dev/null
wait_for_worker_restart "$start_time_before_shutdown"
run_smoke_test

worker_key_before_long_outage=$("${compose[@]}" exec -T redis redis-cli --raw --scan --pattern 'worker:chromium:*' | head -n 1)
start_time_before_long_outage="$(worker_start_time)"
"${compose[@]}" stop redis
wait_for_worker_restart "$start_time_before_long_outage"

worker_logs=$("${compose[@]}" logs worker)
if ! grep -Eq '"exitCode":[[:space:]]*1' <<< "$worker_logs"; then
    echo 'Worker did not report a failed exit after exhausting Redis reconnects.' >&2
    exit 1
fi

"${compose[@]}" start redis
wait_for_redis
wait_for_new_worker_registration "$worker_key_before_long_outage"

echo 'Worker Redis resilience test passed.'
