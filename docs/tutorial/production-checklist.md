# Production Checklist

**Time: 20 minutes**

Before deploying to production, work through this checklist to avoid common pitfalls.

---

## The Checklist

- [ ] Networking is configured correctly
- [ ] Timing values are appropriate
- [ ] Resource limits are set
- [ ] Monitoring is in place
- [ ] You understand restart behavior

---

## 1. Networking

### Verify Internal Communication

The proxy must be able to reach workers at their registered addresses.

**Test from the proxy container:**

```bash
# Get a worker's registered address from Redis
docker compose exec redis redis-cli HGET "worker:chromium:$(docker compose exec redis redis-cli KEYS 'worker:chromium:*' | head -1 | cut -d: -f3)" wsEndpoint

# Test connectivity from proxy
docker compose exec proxy wget -q -O- --timeout=2 http://worker:3131 || echo "Connection failed"
```

### Common Networking Mistakes

| Mistake | Symptom | Fix |
|---------|---------|-----|
| `PRIVATE_HOSTNAME` not resolvable | "No available servers" | Use Docker service names or proper DNS |
| Port mismatch | Connection timeouts | Ensure `PORT` matches registered endpoint |
| Network isolation | Workers register but proxy can't reach them | Put all containers on same network |

### Example Working Configuration

```yaml
# docker-compose.yaml
services:
  proxy:
    networks:
      - grid-network
    environment:
      - REDIS_HOST=redis

  worker:
    networks:
      - grid-network
    environment:
      - REDIS_URL=redis://redis:6379
      - PRIVATE_HOSTNAME=worker  # Must be resolvable from proxy

networks:
  grid-network:
    driver: bridge
```

---

## 2. Timing Configuration

Misconfigured timing causes silent failures. Review these relationships:

### The Dependency Chain

```
Worker Heartbeat (10s)
    ↓ must be less than
Worker Key TTL (30s)
    ↓ must be less than
Shutdown Command TTL (60s)
```

### Verify Your Configuration

| Setting | Location | Default | Minimum Safe Value |
|---------|----------|---------|-------------------|
| `HEARTBEAT_INTERVAL` | Worker | 10s | - |
| `REDIS_KEY_TTL` | Worker | 30s | 2× heartbeat |
| `SHUTDOWN_COMMAND_TTL` | Proxy | 60s | 4× heartbeat |
| `WORKER_SELECT_TIMEOUT` | Proxy | 5s | < HTTP timeout (15s) |

### What Goes Wrong

| Misconfiguration | Result |
|------------------|--------|
| TTL < heartbeat | Workers appear dead while healthy |
| Command TTL < heartbeat | Shutdown commands expire before workers see them |
| Select timeout > HTTP timeout | Connections hang instead of returning 503 |

For details, see [Timing Configuration](../configuration/timing.md).

---

## 3. Resource Limits

Browsers are memory-hungry. Set limits to prevent runaway containers.

### Recommended Limits

```yaml
services:
  worker:
    deploy:
      resources:
        limits:
          memory: 2G  # Adjust based on MAX_CONCURRENT_SESSIONS
          cpus: '1.0'
        reservations:
          memory: 512M
```

### Memory Guidelines

| Concurrent Sessions | Recommended Memory |
|--------------------|-------------------|
| 1-2 | 1 GB |
| 3-5 | 2 GB |
| 6-10 | 4 GB |

These are rough estimates. Monitor actual usage and adjust.

---

## 4. Monitoring

### Essential Metrics

**Proxy metrics** (GET `/metrics`):
```json
{"activeConnections": 12}
```

**Redis queries** for deeper insight:

```bash
# Active connections per worker
redis-cli HGETALL cluster:active_connections

# Lifetime connections per worker
redis-cli HGETALL cluster:lifetime_connections

# Worker status
redis-cli HGET worker:chromium:abc123 status
```

### What to Alert On

| Condition | Threshold | Action |
|-----------|-----------|--------|
| Active connections | > 80% of capacity | Scale up |
| 503 errors | Any sustained | Check workers, scale up |
| Worker restarts | Unusually frequent | Check for crashes |
| Redis latency | > 100ms | Check Redis health |

### Log Levels

For production, use `LOG_LEVEL=info`. Use `debug` only when troubleshooting:

```yaml
environment:
  - LOG_LEVEL=info  # info, warn, error, debug
```

---

## 5. Restart Behavior

Understand what happens when workers restart:

### Normal Restart (Lifetime Limit)

1. Worker hits `MAX_LIFETIME_SESSIONS`
2. Proxy sends shutdown command
3. Worker enters "draining" state
4. Worker waits for active connections to close (up to 30s)
5. Worker exits
6. Docker restarts the container
7. New worker registers with Redis

**Impact**: Minimal. Only one worker restarts at a time due to the staggered selection algorithm.

### Abnormal Restart (Crash)

1. Worker crashes
2. Container restarts immediately
3. Old worker record expires from Redis (after TTL)
4. New worker registers

**Impact**: Active connections on that worker are lost. Other workers continue normally.

### During Restarts

- Active connections on the restarting worker complete normally (during drain)
- New connections route to other workers
- If all workers are restarting simultaneously (shouldn't happen), you get 503 errors

---

## 6. Security Considerations

### Exposed Ports

Only expose the proxy:

```yaml
services:
  proxy:
    ports:
      - "8080:8080"  # Only this should be public

  worker:
    # No ports exposed to host

  redis:
    # No ports exposed to host
```

### TLS Termination

The proxy accepts plain WebSocket. For production, terminate TLS upstream:

```
Client → Load Balancer (TLS) → Proxy (plain WS) → Workers
```

### Network Isolation

Consider putting workers on an isolated network with limited internet access, especially if running untrusted code.

---

## 7. Pre-Flight Test

Before going live, run this test:

```python
import asyncio
from playwright.async_api import async_playwright

async def stress_test():
    """Run multiple concurrent sessions to verify the grid."""
    async with async_playwright() as p:
        async def session(i):
            try:
                browser = await p.chromium.connect(
                    "ws://your-grid:8080",
                    timeout=30000
                )
                page = await browser.new_page()
                await page.goto("https://example.com")
                await browser.close()
                return f"Session {i}: OK"
            except Exception as e:
                return f"Session {i}: FAILED - {e}"

        # Run 20 concurrent sessions
        results = await asyncio.gather(*[session(i) for i in range(20)])

        for result in results:
            print(result)

        failed = sum(1 for r in results if "FAILED" in r)
        print(f"\n{20 - failed}/20 sessions succeeded")

asyncio.run(stress_test())
```

All sessions should succeed. If they don't, check:
1. Worker count (need at least 4 workers for 20 concurrent with default settings)
2. Network connectivity
3. Proxy logs for errors

---

## Quick Checklist

```
□ Workers can reach Redis
□ Proxy can reach workers at PRIVATE_HOSTNAME
□ Timing values follow dependency rules
□ Memory limits are set
□ Only proxy port is exposed
□ Monitoring is configured
□ Stress test passes
```

---

## Next Steps

Ready to deploy? Check the deployment guides:

- **[Docker Compose](../deployment/docker-compose.md)** — Simple deployments
- **[Kubernetes](../deployment/kubernetes.md)** — Production orchestration

Having issues? See **[Troubleshooting](../operations/troubleshooting.md)**.
