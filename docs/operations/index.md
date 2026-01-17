# Operations Guide

Running playwright-distributed in production requires monitoring, troubleshooting skills, and maintenance procedures. This guide covers day-to-day operations.

---

## Operations Guides

### [Monitoring](monitoring.md)

What to watch and how:
- Key metrics to track
- Setting up alerts
- Reading the logs
- Redis monitoring

### [Troubleshooting](troubleshooting.md)

When things go wrong:
- Symptom-based diagnosis
- Common issues and fixes
- Debug procedures
- Getting help

### [Maintenance](maintenance.md)

Keeping things running:
- Rolling updates
- Scaling operations
- Backup and recovery
- Routine tasks

---

## Quick Health Check

Run this to check if everything is working:

```bash
# 1. Check all containers are running
docker compose ps

# 2. Check proxy is accepting connections
curl http://localhost:8080/metrics

# 3. Check workers are registered
docker compose exec redis redis-cli KEYS "worker:*"

# 4. Test end-to-end connection
python -c "
import asyncio
from playwright.async_api import async_playwright

async def test():
    async with async_playwright() as p:
        browser = await p.chromium.connect('ws://localhost:8080', timeout=5000)
        print('Connection successful!')
        await browser.close()

asyncio.run(test())
"
```

If all four pass, your grid is healthy.

---

## Key Metrics at a Glance

| Metric | Healthy | Warning | Critical |
|--------|---------|---------|----------|
| Active connections | < 80% capacity | 80-95% | > 95% |
| Worker count | Stable | Fluctuating | Declining |
| 503 error rate | 0% | < 1% | > 1% |
| Worker restart rate | 1-2/hour per worker | Varies with load | Many simultaneous |
| Redis latency | < 10ms | 10-50ms | > 50ms |

---

## Common Operations

### Check Active Connections

```bash
# Total
curl -s http://localhost:8080/metrics | jq .activeConnections

# Per worker
docker compose exec redis redis-cli HGETALL cluster:active_connections
```

### View Recent Logs

```bash
# Proxy logs
docker compose logs --tail=100 proxy

# Worker logs
docker compose logs --tail=100 worker

# Follow logs in real-time
docker compose logs -f
```

### Scale Workers

```bash
# Scale up
docker compose up -d --scale worker=10

# Scale down
docker compose up -d --scale worker=3
```

### Force Worker Restart

```bash
# Restart all workers
docker compose restart worker

# Restart specific worker (if named)
docker compose restart worker-chromium
```

---

## Incident Response

### "No Available Servers" Errors

1. Check if workers are running: `docker compose ps`
2. Check if workers are registered: `docker compose exec redis redis-cli KEYS "worker:*"`
3. Check worker logs: `docker compose logs worker`
4. See [Troubleshooting](troubleshooting.md#no-available-servers)

### High Latency

1. Check active connections vs capacity
2. Check worker CPU/memory usage
3. Check Redis latency
4. Scale workers if needed

### Worker Crashes

1. Check worker logs for error messages
2. Check memory limits
3. Check for browser-specific issues
4. See [Troubleshooting](troubleshooting.md#workers-crashing)

---

## Next Steps

- **[Monitoring](monitoring.md)** — Set up proper observability
- **[Troubleshooting](troubleshooting.md)** — Debug issues
- **[Maintenance](maintenance.md)** — Routine operations
