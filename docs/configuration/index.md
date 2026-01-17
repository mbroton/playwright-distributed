# Configuration

playwright-distributed is configured entirely through environment variables. This guide covers all available options and how to set them correctly.

---

## Quick Reference

### Proxy

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `REDIS_HOST` | - | Yes | Redis server hostname |
| `REDIS_PORT` | - | Yes | Redis server port |
| `MAX_CONCURRENT_SESSIONS` | 5 | No | Max concurrent connections per worker |
| `MAX_LIFETIME_SESSIONS` | 50 | No | Connections before worker restart |
| `WORKER_SELECT_TIMEOUT` | 5 | No | Seconds to wait for available worker |
| `SHUTDOWN_COMMAND_TTL` | 60 | No | Seconds before shutdown command expires |
| `REAPER_RUN_INTERVAL` | 300 | No | Seconds between reaper runs |
| `DEFAULT_BROWSER_TYPE` | chromium | No | Default browser when not specified |
| `LOG_LEVEL` | info | No | debug, info, warn, error |
| `LOG_FORMAT` | json | No | json or text |

### Worker

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `REDIS_URL` | - | Yes | Redis connection URL |
| `PORT` | - | Yes | Port for WebSocket server |
| `BROWSER_TYPE` | chromium | No | chromium, firefox, or webkit |
| `PRIVATE_HOSTNAME` | - | No | Hostname for proxy to reach worker |
| `HEADLESS` | true | No | Run browser in headless mode |
| `HEARTBEAT_INTERVAL` | 5 | No | Seconds between heartbeats |
| `REDIS_KEY_TTL` | 60 | No | Seconds before worker key expires |
| `REDIS_RETRY_ATTEMPTS` | 5 | No | Times to retry Redis connection |
| `REDIS_RETRY_DELAY` | 3 | No | Seconds between retry attempts |
| `LOG_LEVEL` | info | No | debug, info, warn, error |
| `LOG_FORMAT` | json | No | json or text |

---

## Configuration Guides

### [Proxy Configuration](proxy.md)

All proxy environment variables with detailed explanations, recommendations, and examples.

### [Worker Configuration](worker.md)

All worker environment variables with detailed explanations, recommendations, and examples.

### [Timing Configuration](timing.md)

**Critical reading.** Timing values have dependencies—misconfiguring them causes silent failures. This guide explains the relationships and safe values.

### [Networking](networking.md)

How to configure networking for Docker Compose, Kubernetes, and bare metal deployments. Covers the common `PRIVATE_HOSTNAME` confusion.

---

## Example Configurations

### Development (Local Docker)

```yaml
# docker-compose.yaml
services:
  proxy:
    environment:
      - REDIS_HOST=redis
      - REDIS_PORT=6379
      - LOG_LEVEL=debug
      - LOG_FORMAT=text

  worker:
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker
      - BROWSER_TYPE=chromium
      - LOG_LEVEL=debug
      - LOG_FORMAT=text
```

### Production

```yaml
services:
  proxy:
    environment:
      - REDIS_HOST=redis
      - REDIS_PORT=6379
      - MAX_CONCURRENT_SESSIONS=10
      - MAX_LIFETIME_SESSIONS=100
      - LOG_LEVEL=info
      - LOG_FORMAT=json

  worker:
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker
      - BROWSER_TYPE=chromium
      - HEADLESS=true
      - HEARTBEAT_INTERVAL=10
      - REDIS_KEY_TTL=30
      - LOG_LEVEL=info
      - LOG_FORMAT=json
    deploy:
      resources:
        limits:
          memory: 2G
```

### High-Throughput

```yaml
services:
  proxy:
    environment:
      - MAX_CONCURRENT_SESSIONS=15
      - MAX_LIFETIME_SESSIONS=200
      - WORKER_SELECT_TIMEOUT=10

  worker:
    environment:
      - HEARTBEAT_INTERVAL=5
      - REDIS_KEY_TTL=20
    deploy:
      resources:
        limits:
          memory: 4G
```

---

## Common Mistakes

### Missing Required Variables

```
❌ Error: REDIS_HOST is required
```

Set all required variables. Check the tables above.

### Timing Misconfiguration

```
Workers appear dead even though they're running
```

See [Timing Configuration](timing.md). Key TTL must be greater than heartbeat interval.

### Network Unreachable

```
Proxy can't connect to worker
```

See [Networking](networking.md). The `PRIVATE_HOSTNAME` must be resolvable from the proxy.

---

## Validation

### Proxy

The proxy validates configuration on startup. If validation fails, it exits with an error message.

### Worker

The worker uses Zod for schema validation. Invalid configuration is rejected with detailed error messages:

```
Configuration validation failed: [
  {
    "code": "invalid_type",
    "expected": "string",
    "received": "undefined",
    "path": ["REDIS_URL"],
    "message": "Required"
  }
]
```

---

## Next Steps

- **[Proxy Configuration](proxy.md)** — Detailed proxy settings
- **[Worker Configuration](worker.md)** — Detailed worker settings
- **[Timing Configuration](timing.md)** — Critical timing dependencies
