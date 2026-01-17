# Troubleshooting

This guide helps you diagnose and fix common issues with playwright-distributed.

---

## Quick Diagnosis

| Symptom | Likely Cause | Jump To |
|---------|--------------|---------|
| "No available servers" | No workers or all at capacity | [No Available Servers](#no-available-servers) |
| Connection hangs then fails | Network or timeout issues | [Connection Timeouts](#connection-timeouts) |
| Workers keep restarting | Crashes or config issues | [Workers Crashing](#workers-crashing) |
| High latency | Overloaded or resource constraints | [Performance Issues](#performance-issues) |
| Workers not registering | Redis or network issues | [Registration Issues](#registration-issues) |
| Intermittent failures | Race conditions or instability | [Intermittent Failures](#intermittent-failures) |

---

## No Available Servers

**Error message**: `503 Service Unavailable: no available servers`

### Step 1: Check if Workers Are Running

```bash
docker compose ps
```

All worker containers should show "Up".

**If workers are not running**: Check worker logs for startup errors.

### Step 2: Check if Workers Are Registered

```bash
docker compose exec redis redis-cli KEYS "worker:*:*"
```

Should return at least one worker key.

**If no workers registered**:
- Check `REDIS_URL` is correct
- Check network connectivity to Redis
- Check worker logs for registration errors

### Step 3: Check Worker Status

```bash
# Get a worker key from the previous command
docker compose exec redis redis-cli HGETALL worker:chromium:abc123
```

Check:
- `status` should be "available" (not "draining")
- `lastHeartbeat` should be recent (within last 30 seconds)

**If status is "draining"**: Worker is shutting down, wait for restart.

**If heartbeat is old**: Worker might be stuck, check logs and restart.

### Step 4: Check Capacity

```bash
docker compose exec redis redis-cli HGETALL cluster:active_connections
```

Compare active connections to `MAX_CONCURRENT_SESSIONS × worker_count`.

**If at capacity**: Scale up workers or wait for connections to finish.

### Step 5: Check Browser Type

Make sure you're requesting a browser type that has workers:

```python
# If you request Firefox but only Chromium workers exist
browser = await p.firefox.connect("ws://localhost:8080?browser=firefox")
# This will fail with "no available servers"
```

---

## Connection Timeouts

**Symptom**: Connection hangs for several seconds then fails.

### Step 1: Check Worker Select Timeout

```bash
# Check proxy config
docker compose exec proxy env | grep WORKER_SELECT_TIMEOUT
```

If set too high (> 10s), connections may hang before failing.

### Step 2: Test Network Path

```bash
# From proxy, try to reach worker
docker compose exec proxy wget -q -O- --timeout=2 http://worker:3131

# Check if it times out
```

**If timeout**: Network issue between proxy and worker.

### Step 3: Check PRIVATE_HOSTNAME

```bash
# Get registered endpoint
docker compose exec redis redis-cli HGET worker:chromium:abc123 wsEndpoint
```

The hostname in this endpoint must be resolvable from the proxy.

**If not resolvable**: Set `PRIVATE_HOSTNAME` correctly. See [Networking](../configuration/networking.md).

### Step 4: Check DNS Resolution

```bash
# From proxy container
docker compose exec proxy nslookup worker
```

Should resolve to the worker container's IP.

---

## Workers Crashing

**Symptom**: Workers restart frequently with errors.

### Step 1: Check Worker Logs

```bash
docker compose logs worker --tail=200
```

Look for:
- Error messages
- Stack traces
- Out of memory errors

### Step 2: Check Resource Limits

```bash
docker stats
```

Is the worker hitting memory or CPU limits?

**If memory limit**: Increase limit or reduce `MAX_CONCURRENT_SESSIONS`.

### Step 3: Check Browser-Specific Issues

Some websites crash specific browser versions:

```bash
# Check which browser is crashing
docker compose logs worker | grep -i "crash\|error\|failed"
```

Try:
- Updating the Playwright image
- Testing with a different browser type
- Isolating the problematic URL

### Step 4: Check for Startup Race Condition

If workers crash immediately on startup:

```bash
# Check if Redis is available
docker compose exec worker sh -c "nc -zv redis 6379"
```

**If Redis not reachable**: Worker can't register and may crash.

Increase `REDIS_RETRY_ATTEMPTS` and `REDIS_RETRY_DELAY`.

---

## Performance Issues

**Symptom**: Connections are slow, pages load slowly.

### Step 1: Check Resource Usage

```bash
docker stats --format "table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"
```

**If CPU > 80%**: Workers are overloaded. Scale up or reduce concurrent sessions.

**If Memory near limit**: Reduce concurrent sessions or increase memory limit.

### Step 2: Check Active Connections

```bash
curl http://localhost:8080/metrics
```

**If near capacity**: Scale up workers.

### Step 3: Check Redis Latency

```bash
docker compose exec redis redis-cli --latency
```

**If > 10ms average**: Redis may be overloaded or network is slow.

### Step 4: Check Worker Distribution

```bash
docker compose exec redis redis-cli HGETALL cluster:active_connections
```

**If uneven**: One worker might be stuck or slower than others.

---

## Registration Issues

**Symptom**: Workers are running but not appearing in Redis.

### Step 1: Check Worker Logs

```bash
docker compose logs worker | grep -i "redis\|register"
```

Look for connection errors or registration failures.

### Step 2: Check Redis Connectivity

```bash
# From worker
docker compose exec worker sh -c "nc -zv redis 6379"
```

**If fails**: Check network configuration, Redis is running, `REDIS_URL` is correct.

### Step 3: Check Redis URL Format

```bash
docker compose exec worker env | grep REDIS_URL
```

Should be: `redis://hostname:port` (e.g., `redis://redis:6379`)

### Step 4: Check Key Expiration

```bash
# Check TTL on worker key
docker compose exec redis redis-cli TTL worker:chromium:abc123
```

**If -2 (key doesn't exist)**: Worker never registered or key expired.

**If small positive number**: Key is about to expire. Check `REDIS_KEY_TTL` > `HEARTBEAT_INTERVAL`.

---

## Intermittent Failures

**Symptom**: Sometimes works, sometimes fails unpredictably.

### Step 1: Check for Timing Issues

Review [Timing Configuration](../configuration/timing.md):

```bash
# Worker config
docker compose exec worker env | grep -E 'HEARTBEAT|TTL'

# Proxy config
docker compose exec proxy env | grep -E 'TIMEOUT|TTL'
```

Verify:
- `REDIS_KEY_TTL` > `HEARTBEAT_INTERVAL` (at least 2×)
- `SHUTDOWN_COMMAND_TTL` > `HEARTBEAT_INTERVAL` (at least 4×)

### Step 2: Check for Race Conditions

```bash
# Watch worker count over time
watch -n 1 'docker compose exec -T redis redis-cli KEYS "worker:*:*" | wc -l'
```

**If count fluctuates**: Workers may be restarting too frequently.

### Step 3: Check for Network Instability

```bash
# Monitor connection errors
docker compose logs -f proxy | grep -i "error\|failed"
```

**If frequent errors**: Check network, DNS, or firewall rules.

### Step 4: Enable Debug Logging

```yaml
environment:
  - LOG_LEVEL=debug
```

Restart and watch for patterns in the detailed logs.

---

## Debug Procedures

### Enable Debug Logging

```yaml
# docker-compose.yaml
services:
  proxy:
    environment:
      - LOG_LEVEL=debug

  worker:
    environment:
      - LOG_LEVEL=debug
```

Restart:
```bash
docker compose up -d
```

### Inspect Redis State

```bash
# All keys
docker compose exec redis redis-cli KEYS "*"

# Worker details
docker compose exec redis redis-cli HGETALL worker:chromium:abc123

# Active connections
docker compose exec redis redis-cli HGETALL cluster:active_connections

# Shutdown commands
docker compose exec redis redis-cli KEYS "worker:cmd:*"
```

### Test Connection Manually

```python
import asyncio
from playwright.async_api import async_playwright

async def test():
    async with async_playwright() as p:
        try:
            browser = await p.chromium.connect(
                "ws://localhost:8080",
                timeout=10000  # 10 second timeout
            )
            print("Connected successfully!")

            page = await browser.new_page()
            await page.goto("https://example.com")
            print(f"Page title: {await page.title()}")

            await browser.close()
            print("Test passed!")

        except Exception as e:
            print(f"Error: {e}")

asyncio.run(test())
```

### Check Container Network

```bash
# Inspect network
docker network inspect playwright-distributed_default

# Check DNS resolution from proxy
docker compose exec proxy nslookup worker
docker compose exec proxy nslookup redis

# Check connectivity
docker compose exec proxy nc -zv worker 3131
docker compose exec proxy nc -zv redis 6379
```

---

## Getting Help

If you can't resolve the issue:

1. **Gather information**:
   - Docker Compose file (sanitized)
   - Output of `docker compose ps`
   - Relevant logs (proxy and worker)
   - Steps to reproduce

2. **Check existing issues**:
   - [GitHub Issues](https://github.com/mbroton/playwright-distributed/issues)

3. **Open a new issue** with:
   - Clear description of the problem
   - Expected vs actual behavior
   - Logs and configuration
   - Steps to reproduce

---

## Common Error Messages

| Error | Meaning | Fix |
|-------|---------|-----|
| "no available servers" | No workers can accept connections | Check workers, capacity, browser type |
| "connection refused" | Can't reach target host | Check network, ports, hostnames |
| "timeout" | Operation took too long | Check timeouts, network, resources |
| "ECONNRESET" | Connection dropped unexpectedly | Check worker health, network stability |
| "browser has been closed" | Worker restarted during session | Handle reconnection in client code |

---

## Next Steps

- **[Monitoring](monitoring.md)** — Set up proactive alerting
- **[Maintenance](maintenance.md)** — Routine operations
- **[Configuration](../configuration/index.md)** — Review settings
