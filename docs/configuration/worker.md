# Worker Configuration

Workers are configured entirely through environment variables. This page documents every option.

---

## Required Variables

### `REDIS_URL`

**Type**: URL String
**Required**: Yes

Full Redis connection URL.

```yaml
environment:
  - REDIS_URL=redis://redis:6379
  - REDIS_URL=redis://username:password@redis:6379
  - REDIS_URL=redis://redis:6379/0  # With database number
```

### `PORT`

**Type**: Integer
**Required**: Yes

Port for the worker's WebSocket server.

```yaml
environment:
  - PORT=3131
```

The worker listens on this port for connections from the proxy.

---

## Browser Configuration

### `BROWSER_TYPE`

**Type**: String
**Default**: `chromium`
**Options**: `chromium`, `firefox`, `webkit`

Which browser engine this worker runs.

```yaml
environment:
  - BROWSER_TYPE=chromium  # Chrome/Edge
  - BROWSER_TYPE=firefox   # Firefox
  - BROWSER_TYPE=webkit    # Safari
```

**Note**: Each worker runs one browser type. For multiple browsers, run multiple workers with different `BROWSER_TYPE` values.

### `HEADLESS`

**Type**: Boolean string
**Default**: `true`
**Options**: `true`, `false`

Whether to run the browser without a visible window.

```yaml
environment:
  - HEADLESS=true   # Production (no GUI)
  - HEADLESS=false  # Debugging (shows browser window)
```

**When to use `false`**:
- Debugging automation issues
- Recording video of browser actions
- Development environments with display access

**Note**: `HEADLESS=false` requires a display (X11/Wayland on Linux, or use Xvfb).

---

## Networking

### `PRIVATE_HOSTNAME`

**Type**: String
**Default**: Auto-detected
**Required**: Usually yes in production

The hostname the proxy should use to connect to this worker.

```yaml
environment:
  - PRIVATE_HOSTNAME=worker
  - PRIVATE_HOSTNAME=worker-chromium
  - PRIVATE_HOSTNAME=10.0.1.50
```

**Why this matters**:

When the worker registers with Redis, it tells the proxy: "Connect to me at `ws://PRIVATE_HOSTNAME:PORT/...`"

The proxy then uses this address. If the proxy can't resolve this hostname, connections fail.

**Common patterns**:

| Environment | Value |
|-------------|-------|
| Docker Compose | Service name (e.g., `worker`) |
| Kubernetes | Pod DNS name or service name |
| Bare metal | IP address or DNS name |

**If not set**: The worker tries to auto-detect its hostname, which may not work in containerized environments.

---

## Heartbeat & Health

### `HEARTBEAT_INTERVAL`

**Type**: Integer (seconds)
**Default**: 5

How often the worker sends "I'm alive" signals to Redis.

```yaml
environment:
  - HEARTBEAT_INTERVAL=5   # Every 5 seconds
  - HEARTBEAT_INTERVAL=10  # Every 10 seconds
```

**Guidance**:
- Lower = faster detection of dead workers, more Redis traffic
- Higher = slower detection, less traffic
- Must be less than `REDIS_KEY_TTL`

### `REDIS_KEY_TTL`

**Type**: Integer (seconds)
**Default**: 60

How long the worker's Redis key lives before expiring.

```yaml
environment:
  - REDIS_KEY_TTL=60  # Key expires after 60 seconds
  - REDIS_KEY_TTL=30  # Key expires after 30 seconds
```

**Critical rule**: `REDIS_KEY_TTL` must be greater than `HEARTBEAT_INTERVAL`.

Why? Each heartbeat refreshes the TTL. If TTL < heartbeat interval, the key expires before the next heartbeat, making the worker appear dead.

**Recommended**: `REDIS_KEY_TTL = 2× to 3× HEARTBEAT_INTERVAL`

**Examples**:

| Heartbeat | Safe TTL |
|-----------|----------|
| 5s | 15-20s |
| 10s | 30s |
| 15s | 45s |

---

## Redis Connection

### `REDIS_RETRY_ATTEMPTS`

**Type**: Integer
**Default**: 5

How many times to retry connecting to Redis on startup.

```yaml
environment:
  - REDIS_RETRY_ATTEMPTS=5
```

If Redis isn't available immediately (common in Docker startup race conditions), the worker retries.

### `REDIS_RETRY_DELAY`

**Type**: Integer (seconds)
**Default**: 3

Seconds to wait between Redis connection retries.

```yaml
environment:
  - REDIS_RETRY_DELAY=3
```

**Total startup tolerance**: `REDIS_RETRY_ATTEMPTS × REDIS_RETRY_DELAY` seconds.

With defaults: 5 × 3 = 15 seconds for Redis to become available.

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

### `LOG_FORMAT`

**Type**: String
**Default**: `json`
**Options**: `json`, `text`

Output format for logs.

```yaml
environment:
  - LOG_FORMAT=json   # Production
  - LOG_FORMAT=text   # Development
```

---

## Complete Example

```yaml
services:
  worker:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
    environment:
      # Required
      - REDIS_URL=redis://redis:6379
      - PORT=3131

      # Browser
      - BROWSER_TYPE=chromium
      - HEADLESS=true

      # Networking
      - PRIVATE_HOSTNAME=worker

      # Heartbeat
      - HEARTBEAT_INTERVAL=10
      - REDIS_KEY_TTL=30

      # Redis connection
      - REDIS_RETRY_ATTEMPTS=5
      - REDIS_RETRY_DELAY=3

      # Logging
      - LOG_LEVEL=info
      - LOG_FORMAT=json
```

---

## Multi-Browser Setup

To run multiple browser types, create multiple worker services:

```yaml
services:
  worker-chromium:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - BROWSER_TYPE=chromium
      - PRIVATE_HOSTNAME=worker-chromium

  worker-firefox:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - BROWSER_TYPE=firefox
      - PRIVATE_HOSTNAME=worker-firefox

  worker-webkit:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - BROWSER_TYPE=webkit
      - PRIVATE_HOSTNAME=worker-webkit
```

Each worker registers with its browser type, and the proxy routes `?browser=firefox` requests to Firefox workers.

---

## Validation

The worker validates configuration on startup using Zod schemas:

```
Configuration validation failed: [
  {
    "code": "invalid_url",
    "path": ["REDIS_URL"],
    "message": "Invalid Redis URL"
  }
]
```

Invalid configuration causes the worker to exit immediately.

---

## See Also

- **[Proxy Configuration](proxy.md)** — Proxy-side settings
- **[Timing Configuration](timing.md)** — How timing values interact
- **[Networking](networking.md)** — PRIVATE_HOSTNAME explained
