#!/usr/bin/env bash

set -euo pipefail

compose=(docker compose -f docker-compose.local.yaml)
if [[ -n "${COMPOSE_OVERRIDE_FILE:-}" ]]; then
    compose+=(-f "$COMPOSE_OVERRIDE_FILE")
fi
ws_endpoint="${WS_ENDPOINT:-ws://127.0.0.1:8080}"

worker_container_id() {
    "${compose[@]}" ps -q worker
}

worker_restart_count() {
    docker inspect --format '{{.RestartCount}}' "$(worker_container_id)"
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
    local previous_count="$1"
    local current_count

    for _ in {1..30}; do
        current_count=$(worker_restart_count 2>/dev/null || true)
        if [[ -n "$current_count" ]] && (( current_count > previous_count )); then
            return
        fi
        sleep 1
    done

    echo 'Worker did not restart.' >&2
    return 1
}

wait_for_worker_log() {
    local expected="$1"
    local worker_logs

    for _ in {1..15}; do
        worker_logs=$("${compose[@]}" logs worker)
        if [[ "$worker_logs" == *"$expected"* ]]; then
            return
        fi
        sleep 1
    done

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

initial_restart_count="$(worker_restart_count)"

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

if (( $(worker_restart_count) != initial_restart_count )); then
    echo 'Worker restarted during a recoverable Redis outage.' >&2
    exit 1
fi

if ! wait_for_worker_log 'Redis state restored after reconnect'; then
    echo 'Worker did not restore its Redis state after reconnecting.' >&2
    exit 1
fi

worker_key=$("${compose[@]}" exec -T redis redis-cli --raw --scan --pattern 'worker:chromium:*' | head -n 1)
if [[ -z "$worker_key" ]]; then
    echo 'No Chromium worker registration found in Redis.' >&2
    exit 1
fi

restart_count_before_shutdown="$(worker_restart_count)"
command_channel="worker:cmd:${worker_key#worker:}"
"${compose[@]}" exec -T redis redis-cli publish "$command_channel" shutdown >/dev/null
wait_for_worker_restart "$restart_count_before_shutdown"
run_smoke_test

worker_key_before_long_outage=$("${compose[@]}" exec -T redis redis-cli --raw --scan --pattern 'worker:chromium:*' | head -n 1)
restart_count_before_long_outage="$(worker_restart_count)"
"${compose[@]}" stop redis
wait_for_worker_restart "$restart_count_before_long_outage"

worker_logs=$("${compose[@]}" logs worker)
if ! grep -Eq '"exitCode":[[:space:]]*1' <<< "$worker_logs"; then
    echo 'Worker did not report a failed exit after exhausting Redis reconnects.' >&2
    exit 1
fi

"${compose[@]}" start redis
wait_for_redis
wait_for_new_worker_registration "$worker_key_before_long_outage"

echo 'Worker Redis resilience test passed.'
