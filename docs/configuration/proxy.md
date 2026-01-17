# Proxy Configuration

The proxy is configured entirely through environment variables. This page documents every option.

---

## Required Variables

### `REDIS_HOST`

**Type**: String
**Required**: Yes

The hostname or IP address of the Redis server.

```yaml
environment:
  - REDIS_HOST=redis          # Docker service name
  - REDIS_HOST=10.0.1.50      # IP address
  - REDIS_HOST=redis.internal # DNS name
```

### `REDIS_PORT`

**Type**: Integer
**Required**: Yes

The port Redis is listening on.

```yaml
environment:
  - REDIS_PORT=6379  # Standard Redis port
```

---

## Worker Management

### `MAX_CONCURRENT_SESSIONS`

**Type**: Integer
**Default**: 5

Maximum number of simultaneous connections a single worker can handle.

```yaml
environment:
  - MAX_CONCURRENT_SESSIONS=5
```

**Guidance**:
- Each concurrent session uses memory (browser context)
- Higher values = more capacity per worker, but more memory usage
- Start with 5, increase if workers have headroom
- Monitor worker memory usage to find the right value

**Example scenarios**:

| Value | Use Case |
|-------|----------|
| 3 | Memory-constrained workers |
| 5 | Default, works for most cases |
| 10 | High-memory workers (4GB+) |
| 15+ | Dedicated high-capacity workers |

### `MAX_LIFETIME_SESSIONS`

**Type**: Integer
**Default**: 50

Total connections a worker handles before restarting.

```yaml
environment:
  - MAX_LIFETIME_SESSIONS=50
```

**Guidance**:
- Workers restart to prevent memory leaks from accumulating
- Lower values = more frequent restarts = fresher memory state
- Higher values = fewer restarts = more stability
- This affects the worker selection algorithm (see [Worker Selection](../concepts/worker-selection.md))

**Example scenarios**:

| Value | Use Case |
|-------|----------|
| 30 | High-throughput, short sessions |
| 50 | Default, balanced |
| 100 | Long-running sessions |
| 200+ | Stable workloads, minimal restarts |

### `WORKER_SELECT_TIMEOUT`

**Type**: Integer (seconds)
**Default**: 5

How long to wait for an available worker before returning 503.

```yaml
environment:
  - WORKER_SELECT_TIMEOUT=5
```

**Guidance**:
- If no workers are available, the proxy waits this long before failing
- Too short: Clients get 503 errors during brief capacity crunches
- Too long: Clients wait a long time before learning about capacity issues
- Must be less than HTTP write timeout (15s hardcoded)

**Example scenarios**:

| Value | Use Case |
|-------|----------|
| 2 | Fail fast, client handles retry |
| 5 | Default, reasonable wait |
| 10 | High-load systems with queuing |

### `SHUTDOWN_COMMAND_TTL`

**Type**: Integer (seconds)
**Default**: 60

How long a shutdown command waits in Redis before expiring.

```yaml
environment:
  - SHUTDOWN_COMMAND_TTL=60
```

**Guidance**:
- When a worker hits lifetime limit, proxy sends a shutdown command
- Worker checks for commands during each heartbeat
- Must be greater than `HEARTBEAT_INTERVAL` (worker setting)
- Recommended: 4× the heartbeat interval

**What happens if too short**: Command expires before worker sees it, worker never restarts.

### `REAPER_RUN_INTERVAL`

**Type**: Integer (seconds)
**Default**: 300

How often the proxy cleans up stale worker records.

```yaml
environment:
  - REAPER_RUN_INTERVAL=300  # Every 5 minutes
```

**Guidance**:
- The reaper removes workers that stopped sending heartbeats
- Too frequent: Unnecessary CPU usage
- Too infrequent: Stale records accumulate longer
- 5 minutes is usually fine

---

## Browser Configuration

### `DEFAULT_BROWSER_TYPE`

**Type**: String
**Default**: `chromium`
**Options**: `chromium`, `firefox`, `webkit`

The browser type to use when clients don't specify one.

```yaml
environment:
  - DEFAULT_BROWSER_TYPE=chromium
```

When a client connects without `?browser=...`, this type is used.

---

## Logging

### `LOG_LEVEL`

**Type**: String
**Default**: `info`
**Options**: `debug`, `info`, `warn`, `error`

Controls log verbosity.

```yaml
environment:
  - LOG_LEVEL=info    # Production
  - LOG_LEVEL=debug   # Troubleshooting
```

**Level details**:

| Level | Includes |
|-------|----------|
| `error` | Errors only |
| `warn` | Warnings and errors |
| `info` | Normal operations, warnings, errors |
| `debug` | Everything including connection details |

### `LOG_FORMAT`

**Type**: String
**Default**: `json`
**Options**: `json`, `text`

Output format for logs.

```yaml
environment:
  - LOG_FORMAT=json   # Production (machine-readable)
  - LOG_FORMAT=text   # Development (human-readable)
```

**JSON format** (default):
```json
{"level":"info","msg":"Client connected","time":"2024-01-15T10:30:00Z","worker":"chromium:abc123"}
```

**Text format**:
```
INFO[2024-01-15T10:30:00Z] Client connected worker=chromium:abc123
```

---

## Complete Example

```yaml
services:
  proxy:
    image: ghcr.io/mbroton/playwright-distributed-proxy:latest
    ports:
      - "8080:8080"
    environment:
      # Required
      - REDIS_HOST=redis
      - REDIS_PORT=6379

      # Worker management
      - MAX_CONCURRENT_SESSIONS=10
      - MAX_LIFETIME_SESSIONS=100
      - WORKER_SELECT_TIMEOUT=5
      - SHUTDOWN_COMMAND_TTL=60
      - REAPER_RUN_INTERVAL=300

      # Browser
      - DEFAULT_BROWSER_TYPE=chromium

      # Logging
      - LOG_LEVEL=info
      - LOG_FORMAT=json
```

---

## Validation

The proxy validates all configuration on startup:

```go
if cfg.RedisHost == "" {
    return nil, fmt.Errorf("REDIS_HOST is required")
}
if cfg.RedisPort == 0 {
    return nil, fmt.Errorf("REDIS_PORT is required")
}
```

If validation fails, the proxy exits with an error message.

---

## See Also

- **[Worker Configuration](worker.md)** — Worker-side settings
- **[Timing Configuration](timing.md)** — How timing values interact
- **[Troubleshooting](../operations/troubleshooting.md)** — Common configuration issues
