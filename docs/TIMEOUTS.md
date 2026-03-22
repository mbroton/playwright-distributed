# Timing and TTL Dependencies

This document lists the important time-based settings in the current implementation and how they interact. If these values drift too far apart, the proxy can route to stale workers, delay connection failures longer than expected, or shut down workers too aggressively.

## Summary

| Parameter | Location | Default | Depends On | Notes |
|---|---|---|---|---|
| Worker Heartbeat Interval | `worker/src/config.ts` | 5s | - | How often a healthy worker refreshes its Redis record. |
| Worker Key TTL | `worker/src/config.ts` | 60s | Worker Heartbeat | Worker metadata disappears after this if heartbeats stop. |
| Selector Freshness Threshold | `proxy/internal/redis/selector.lua` | 60s | Worker Heartbeat | Hardcoded recent-heartbeat check used for worker eligibility. |
| Shutdown Command TTL | `proxy/pkg/config/config.go` | 60s | Worker Heartbeat | Must outlive at least one worker heartbeat cycle. |
| Worker Drain Timeout | `worker/src/index.ts` | 300s (5m) | - | Hardcoded safety net while a draining worker waits for active connections to reach zero. |
| Reaper Run Interval | `proxy/pkg/config/config.go` | 300s (5m) | Worker Key TTL | Reaper removes stale connection counters after worker keys have already expired. |
| Proxy Read Header Timeout | `proxy/pkg/config/config.go` | 5s | HTTP ReadHeaderTimeout | Bounds request header read before the handler runs. |
| Proxy Worker Selection Timeout | `proxy/pkg/config/config.go` | 5s | HTTP WriteTimeout | Bounds how long the proxy retries until a matching worker becomes eligible. |
| Proxy Connect Timeout | `proxy/pkg/config/config.go` | 5s | HTTP WriteTimeout | Bounds backend dial plus client upgrade after a worker has already been selected. |
| HTTP ReadHeaderTimeout | `proxy/internal/proxy/server.go` | 5s (derived) | `PROXY_READ_HEADER_TIMEOUT` | Computed as `PROXY_READ_HEADER_TIMEOUT`. |
| HTTP WriteTimeout | `proxy/internal/proxy/server.go` | 6s (derived) | `PROXY_WORKER_SELECTION_TIMEOUT`, `PROXY_CONNECT_TIMEOUT` | Computed as `max(PROXY_WORKER_SELECTION_TIMEOUT, PROXY_CONNECT_TIMEOUT) + 1s`, then refreshed when the handler enters the connect phase. |

---

## Detailed Explanations

### 1. Worker Heartbeat Interval
- **Purpose**: How often a worker refreshes `lastHeartbeat` and the TTL on its Redis record.
- **File**: `worker/src/config.ts` (`server.heartbeatInterval`)
- **Default**: `5` seconds (`5000` ms at runtime)
- **Dependency Rule**: This is the base cadence other worker-liveness values should be compared against.

### 2. Worker Key TTL
- **Purpose**: The TTL for the main `worker:{browserType}:{id}` Redis hash.
- **File**: `worker/src/config.ts` (`redis.keyTtl`)
- **Default**: `60` seconds
- **Dependency Rule**: Must be greater than `Worker Heartbeat Interval`.
- **Rationale**: The worker key is the source of truth for whether a worker still exists. If this TTL is too short, healthy workers can disappear and be treated as dead.
- **Recommendation**: Keep this comfortably above the heartbeat interval, ideally several multiples rather than only one missed heartbeat.

### 3. Selector Freshness Threshold
- **Purpose**: The selector refuses workers whose `lastHeartbeat` is older than the hardcoded freshness window, even if the worker key still exists.
- **File**: `proxy/internal/redis/selector.lua`
- **Default**: `60` seconds
- **Dependency Rule**: Should remain greater than `Worker Heartbeat Interval`.
- **Rationale**: This is separate from key expiry. It filters stale workers out of routing before the reaper has cleaned up counters.
- **Recommendation**: Keep it aligned with, or slightly below, the worker key TTL unless this value is made configurable later.

### 4. Shutdown Command TTL
- **Purpose**: The TTL for `worker:cmd:{browserType}:{id}` when the proxy asks a worker to drain and shut down.
- **File**: `proxy/pkg/config/config.go` (`SHUTDOWN_COMMAND_TTL`)
- **Default**: `60` seconds
- **Dependency Rule**: Must be greater than `Worker Heartbeat Interval`.
- **Rationale**: The worker checks for commands during heartbeat processing. If the command expires first, the drain request can be missed.

### 5. Worker Drain Timeout
- **Purpose**: Maximum time a draining worker waits for active connections to reach zero before forcing shutdown.
- **File**: `worker/src/index.ts` (hardcoded)
- **Default**: `300` seconds (5 minutes)
- **Dependencies**: None directly, but it should be long enough for normal browser sessions to close cleanly.

### 6. Reaper Run Interval
- **Purpose**: How often the proxy scans for stale counter entries.
- **File**: `proxy/pkg/config/config.go` (`REAPER_RUN_INTERVAL`)
- **Default**: `300` seconds (5 minutes)
- **Dependency Rule**: This is only useful after worker keys have already expired.
- **Rationale**: The reaper does not use its own stale threshold. It simply removes `cluster:active_connections` and `cluster:lifetime_connections` entries whose worker key no longer exists.

### 7. Proxy Read Header Timeout
- **Purpose**: The maximum time the server allows for reading request headers before the handler runs.
- **File**: `proxy/pkg/config/config.go` (`PROXY_READ_HEADER_TIMEOUT`)
- **Default**: `5` seconds
- **Dependency Rule**: `HTTP ReadHeaderTimeout` is derived from this value.
- **Rationale**: This keeps slow or broken clients from tying up the server before the proxy starts its own selection logic.

### 8. Proxy Worker Selection Timeout
- **Purpose**: The maximum time the proxy retries until a matching worker becomes eligible.
- **File**: `proxy/pkg/config/config.go` (`PROXY_WORKER_SELECTION_TIMEOUT`)
- **Default**: `5` seconds
- **Dependency Rule**: `HTTP WriteTimeout` must be long enough to return a structured timeout response if this phase fails.
- **Rationale**: This is the main user-facing queue timeout. When it expires, the client-visible error is `worker selection timed out`.

### 9. Proxy Connect Timeout
- **Purpose**: The maximum time the proxy allows for backend dial and client upgrade after a worker is already selected.
- **File**: `proxy/pkg/config/config.go` (`PROXY_CONNECT_TIMEOUT`)
- **Default**: `5` seconds
- **Dependency Rule**: `HTTP WriteTimeout` must be long enough to return a structured timeout response if this phase fails.
- **Rationale**: This now covers only the post-selection handoff. If the timeout expires before Gorilla starts the client upgrade, the proxy returns `connect timed out after selecting worker`. If the timeout is hit during Gorilla's handshake write, the proxy logs and cleans up server-side, then closes the connection. An actual selected-worker/backend failure returns `selected worker unavailable`.

### 10. HTTP ReadHeaderTimeout
- **Purpose**: Server-side deadline for reading request headers before the handler runs.
- **File**: `proxy/internal/proxy/server.go`
- **Default**: `5` seconds when `PROXY_READ_HEADER_TIMEOUT=5`
- **Dependency Rule**: Computed as `PROXY_READ_HEADER_TIMEOUT`.
- **Rationale**: This is the HTTP server counterpart of the read-header phase and stays independent from worker queue wait and connect handoff timing.

### 11. HTTP WriteTimeout
- **Purpose**: Server-side HTTP write deadline during the pre-upgrade phase.
- **File**: `proxy/internal/proxy/server.go`
- **Default**: `6` seconds when `PROXY_WORKER_SELECTION_TIMEOUT=5` and `PROXY_CONNECT_TIMEOUT=5`
- **Dependency Rule**: Computed as `max(PROXY_WORKER_SELECTION_TIMEOUT, PROXY_CONNECT_TIMEOUT) + 1s`, and refreshed when the handler transitions from selection to connect.
- **Rationale**: The selection and connect phases are request-scoped and sequential. The handler refreshes the write deadline before starting the connect phase so a long worker wait does not consume the ability to return a post-selection timeout response on the same keep-alive request. Once Gorilla hijacks the socket for the client upgrade, the proxy no longer guarantees a structured JSON HTTP error body.
