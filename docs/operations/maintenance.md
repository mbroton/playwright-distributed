# Maintenance

This guide covers routine maintenance tasks for playwright-distributed.

---

## Routine Tasks

### Daily

- [ ] Check active connections and capacity utilization
- [ ] Review error logs for anomalies
- [ ] Verify worker count matches expected

### Weekly

- [ ] Check for image updates
- [ ] Review resource usage trends
- [ ] Clean up old logs

### Monthly

- [ ] Update container images
- [ ] Review and tune configuration
- [ ] Capacity planning review

---

## Rolling Updates

Update workers without downtime by rolling them one at a time.

### Docker Compose

```bash
# Pull new images
docker compose pull

# Restart workers one at a time (manual rolling)
docker compose up -d --no-deps worker

# Workers will restart sequentially due to staggered selection
```

For more control:

```bash
# Get worker container IDs
docker compose ps -q worker

# Restart one at a time with delay
for id in $(docker compose ps -q worker); do
  docker restart $id
  sleep 30  # Wait for new worker to register
done
```

### Kubernetes

Kubernetes handles rolling updates automatically:

```bash
# Update image
kubectl set image deployment/playwright-worker \
  worker=ghcr.io/mbroton/playwright-distributed-worker:new-version \
  -n playwright

# Watch rollout
kubectl rollout status deployment/playwright-worker -n playwright
```

Configure update strategy:

```yaml
spec:
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 1
```

---

## Scaling Operations

### Scale Up (Emergency)

When you need more capacity quickly:

```bash
# Docker Compose
docker compose up -d --scale worker=10

# Kubernetes
kubectl scale deployment/playwright-worker --replicas=10 -n playwright
```

New workers register within seconds (heartbeat interval).

### Scale Down (Cost Saving)

Scale down gracefully to avoid dropping connections:

```bash
# Check active connections first
curl http://localhost:8080/metrics

# Scale down when utilization is low
docker compose up -d --scale worker=3
```

Workers in the pool but over the new limit will:
1. Finish existing connections
2. Stop accepting new connections (via selection algorithm)
3. Eventually hit lifetime limit and not restart

For immediate scale down, remove specific containers:

```bash
# Get container IDs
docker compose ps -q worker

# Remove extras (keep 3)
docker stop $(docker compose ps -q worker | tail -n +4)
```

---

## Backup and Recovery

### What to Back Up

playwright-distributed is mostly stateless. The only persistent data is in Redis:

| Data | Importance | Backup? |
|------|------------|---------|
| Worker registry | Regenerates automatically | No |
| Active connections | Transient | No |
| Lifetime counters | Nice to have | Optional |

**In practice**: You typically don't need Redis backups. Workers re-register when they start.

### Redis Persistence (Optional)

If you want counters to survive Redis restarts:

```yaml
services:
  redis:
    command: redis-server --appendonly yes
    volumes:
      - redis-data:/data

volumes:
  redis-data:
```

### Recovery Procedure

If everything crashes:

1. Start Redis first
2. Start proxy
3. Start workers
4. Workers re-register within `HEARTBEAT_INTERVAL` seconds
5. System is operational

```bash
docker compose up -d redis
sleep 5
docker compose up -d proxy
docker compose up -d worker
```

---

## Log Management

### Log Rotation (Docker)

Configure in `docker-compose.yaml`:

```yaml
services:
  proxy:
    logging:
      driver: "json-file"
      options:
        max-size: "10m"
        max-file: "5"
```

### Clean Old Logs

```bash
# Remove old container logs (Docker)
docker system prune --volumes

# Or specifically logs
truncate -s 0 /var/lib/docker/containers/*/*-json.log
```

### Log Aggregation

For production, send logs to a centralized system:

```yaml
services:
  proxy:
    logging:
      driver: fluentd
      options:
        fluentd-address: "localhost:24224"
        tag: "playwright.proxy"
```

---

## Image Updates

### Check for Updates

```bash
# Docker Hub / GHCR
docker pull ghcr.io/mbroton/playwright-distributed-proxy:latest
docker pull ghcr.io/mbroton/playwright-distributed-worker:latest
```

### Update Procedure

1. **Review changelog** for breaking changes
2. **Test in staging** environment
3. **Update production** during low-traffic period

```bash
# Pull new images
docker compose pull

# Rolling restart
docker compose up -d
```

### Pin Versions (Recommended)

For production, pin specific versions:

```yaml
services:
  proxy:
    image: ghcr.io/mbroton/playwright-distributed-proxy:v1.2.3

  worker:
    image: ghcr.io/mbroton/playwright-distributed-worker:v1.2.3
```

---

## Configuration Changes

### Non-Breaking Changes

These can be applied without restart:

```bash
# Scale workers
docker compose up -d --scale worker=5
```

### Breaking Changes

These require restart:

```bash
# Update environment variables
# Edit docker-compose.yaml, then:
docker compose up -d
```

### Timing Configuration Changes

**Be careful** with timing changes. Follow dependencies:

1. If increasing `HEARTBEAT_INTERVAL`:
   - Increase `REDIS_KEY_TTL` first
   - Then increase `HEARTBEAT_INTERVAL`

2. If decreasing `REDIS_KEY_TTL`:
   - Decrease `HEARTBEAT_INTERVAL` first
   - Then decrease `REDIS_KEY_TTL`

See [Timing Configuration](../configuration/timing.md).

---

## Health Checks

### Manual Health Check

```bash
#!/bin/bash
# health-check.sh

echo "Checking proxy..."
curl -f http://localhost:8080/metrics || echo "FAIL: Proxy not responding"

echo "Checking Redis..."
docker compose exec -T redis redis-cli PING || echo "FAIL: Redis not responding"

echo "Checking workers..."
WORKERS=$(docker compose exec -T redis redis-cli KEYS "worker:*:*" | wc -l)
echo "Workers registered: $WORKERS"
[ "$WORKERS" -gt 0 ] || echo "FAIL: No workers registered"

echo "End-to-end test..."
python3 -c "
import asyncio
from playwright.async_api import async_playwright

async def test():
    async with async_playwright() as p:
        browser = await p.chromium.connect('ws://localhost:8080', timeout=5000)
        await browser.close()
        print('OK')

asyncio.run(test())
" || echo "FAIL: End-to-end connection failed"
```

### Automated Health Checks

Add to cron:

```bash
# Check every 5 minutes
*/5 * * * * /path/to/health-check.sh >> /var/log/playwright-health.log 2>&1
```

---

## Troubleshooting During Maintenance

### Workers Not Coming Back After Restart

```bash
# Check if Redis is accessible
docker compose exec worker sh -c "nc -zv redis 6379"

# Check worker logs
docker compose logs worker --tail=50
```

### Connections Dropping During Update

Normal during rolling updates. Clients should handle reconnection:

```python
async def connect_with_retry(url, max_retries=3):
    for attempt in range(max_retries):
        try:
            return await p.chromium.connect(url)
        except Exception:
            if attempt < max_retries - 1:
                await asyncio.sleep(2)
            else:
                raise
```

### High CPU After Restart

Workers launching browsers cause CPU spike. This is normal and settles within 30 seconds.

---

## Disaster Recovery

### Complete System Failure

1. **Diagnose** the failure (infrastructure, network, configuration)
2. **Fix** the root cause
3. **Start services** in order: Redis → Proxy → Workers
4. **Verify** with health check
5. **Document** the incident

### Single Component Failure

| Component | Recovery |
|-----------|----------|
| Proxy | Restart; stateless, instant recovery |
| Worker | Restart; re-registers automatically |
| Redis | Restart; workers re-register, counters reset |

### Network Partition

If proxy can't reach workers:

1. Fix network issue
2. Workers continue running
3. Connections resume once network recovers
4. No restart needed

---

## Maintenance Windows

### Planning a Maintenance Window

1. **Announce** to users
2. **Drain** traffic gradually
3. **Perform** maintenance
4. **Verify** functionality
5. **Restore** traffic

### Zero-Downtime Maintenance

For most operations, no maintenance window needed:

- Rolling updates
- Scaling operations
- Configuration changes (most)

Maintenance window needed for:

- Major version upgrades
- Redis migration
- Network changes

---

## Checklist

### Pre-Maintenance

```
□ Backup configuration files
□ Note current state (workers, connections)
□ Announce maintenance (if needed)
□ Have rollback plan ready
```

### Post-Maintenance

```
□ Verify all services running
□ Check worker registration
□ Test end-to-end connection
□ Monitor for errors
□ Update documentation if config changed
```

---

## Next Steps

- **[Monitoring](monitoring.md)** — Track system health
- **[Troubleshooting](troubleshooting.md)** — Fix issues
- **[Scaling](../deployment/scaling.md)** — Capacity planning
