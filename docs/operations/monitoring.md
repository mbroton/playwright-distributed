# Monitoring

Effective monitoring helps you catch issues before they impact users. This guide covers what to monitor and how.

---

## Built-in Metrics

### Proxy Metrics Endpoint

```bash
curl http://localhost:8080/metrics
```

Returns:
```json
{
  "activeConnections": 12
}
```

This shows total active connections across all workers.

### Redis Queries

More detailed metrics are available by querying Redis directly.

**Active connections per worker**:
```bash
docker compose exec redis redis-cli HGETALL cluster:active_connections
```

**Lifetime connections per worker**:
```bash
docker compose exec redis redis-cli HGETALL cluster:lifetime_connections
```

**Worker details**:
```bash
docker compose exec redis redis-cli HGETALL worker:chromium:abc123
```

**All registered workers**:
```bash
docker compose exec redis redis-cli KEYS "worker:*:*"
```

---

## Key Metrics to Track

### Connection Metrics

| Metric | Source | Alert Threshold |
|--------|--------|-----------------|
| Total active connections | `/metrics` | > 80% of capacity |
| Connection rate | Log analysis | Sudden spikes |
| 503 error rate | Proxy logs | > 0.1% |
| Connection duration | Log analysis | Unusually long/short |

### Worker Metrics

| Metric | Source | Alert Threshold |
|--------|--------|-----------------|
| Worker count | Redis KEYS | Below minimum |
| Worker heartbeat age | Redis HGET | > 30 seconds |
| Worker status | Redis HGET | "draining" for too long |
| Lifetime connections | Redis HGET | Near MAX_LIFETIME |

### System Metrics

| Metric | Source | Alert Threshold |
|--------|--------|-----------------|
| Worker CPU usage | Docker stats | > 80% sustained |
| Worker memory usage | Docker stats | > 80% of limit |
| Redis memory | Redis INFO | > 80% of limit |
| Redis latency | Redis PING | > 50ms |

---

## Monitoring Scripts

### Check Cluster Health

```bash
#!/bin/bash
# health-check.sh

echo "=== Cluster Health Check ==="

# Check proxy
ACTIVE=$(curl -s http://localhost:8080/metrics | jq -r '.activeConnections')
echo "Active connections: $ACTIVE"

# Check workers
WORKERS=$(docker compose exec -T redis redis-cli KEYS "worker:*:*" | wc -l)
echo "Registered workers: $WORKERS"

# Check capacity
if [ "$WORKERS" -gt 0 ]; then
    MAX_CONCURRENT=${MAX_CONCURRENT_SESSIONS:-5}
    CAPACITY=$((WORKERS * MAX_CONCURRENT))
    UTILIZATION=$((ACTIVE * 100 / CAPACITY))
    echo "Capacity: $CAPACITY"
    echo "Utilization: $UTILIZATION%"
fi

# Check for stale workers
echo ""
echo "=== Worker Status ==="
docker compose exec -T redis redis-cli KEYS "worker:*:*" | while read key; do
    HEARTBEAT=$(docker compose exec -T redis redis-cli HGET "$key" lastHeartbeat)
    NOW=$(date +%s)
    AGE=$((NOW - HEARTBEAT))
    STATUS=$(docker compose exec -T redis redis-cli HGET "$key" status)
    echo "$key: status=$STATUS, heartbeat_age=${AGE}s"
done
```

### Watch Connections in Real-Time

```bash
#!/bin/bash
# watch-connections.sh

watch -n 2 '
echo "=== Active Connections ==="
docker compose exec -T redis redis-cli HGETALL cluster:active_connections

echo ""
echo "=== Lifetime Connections ==="
docker compose exec -T redis redis-cli HGETALL cluster:lifetime_connections

echo ""
echo "=== Total ==="
curl -s http://localhost:8080/metrics
'
```

---

## Log Analysis

### Proxy Log Patterns

**Successful connection**:
```json
{"level":"info","msg":"Client connected","worker":"chromium:abc123","time":"..."}
```

**No workers available**:
```json
{"level":"warn","msg":"No available workers","browser":"chromium","time":"..."}
```

**Worker shutdown triggered**:
```json
{"level":"info","msg":"Triggering worker shutdown","worker":"chromium:abc123","time":"..."}
```

### Worker Log Patterns

**Successful registration**:
```json
{"level":"info","msg":"Registered with Redis","workerId":"abc123","time":"..."}
```

**Heartbeat sent**:
```json
{"level":"debug","msg":"Heartbeat sent","time":"..."}
```

**Shutdown received**:
```json
{"level":"info","msg":"Shutdown command received, entering drain mode","time":"..."}
```

### Log Queries

```bash
# Find 503 errors
docker compose logs proxy | grep -i "no available"

# Find worker restarts
docker compose logs worker | grep -i "shutdown\|drain"

# Find connection errors
docker compose logs proxy | grep -i "error\|failed"
```

---

## Prometheus Integration

Export metrics to Prometheus for long-term storage and alerting.

### Custom Exporter (Example)

```python
# metrics_exporter.py
from prometheus_client import start_http_server, Gauge
import redis
import time

# Metrics
active_connections = Gauge('playwright_active_connections_total', 'Total active connections')
worker_count = Gauge('playwright_worker_count', 'Number of registered workers', ['browser_type'])
worker_lifetime = Gauge('playwright_worker_lifetime', 'Lifetime connections per worker', ['worker_id'])

def collect_metrics():
    r = redis.Redis(host='redis', port=6379)

    # Active connections
    total = sum(int(v) for v in r.hgetall('cluster:active_connections').values())
    active_connections.set(total)

    # Worker count by browser
    workers = r.keys('worker:*:*')
    browser_counts = {}
    for w in workers:
        parts = w.decode().split(':')
        browser = parts[1]
        browser_counts[browser] = browser_counts.get(browser, 0) + 1

    for browser, count in browser_counts.items():
        worker_count.labels(browser_type=browser).set(count)

    # Lifetime per worker
    lifetimes = r.hgetall('cluster:lifetime_connections')
    for worker_id, lifetime in lifetimes.items():
        worker_lifetime.labels(worker_id=worker_id.decode()).set(int(lifetime))

if __name__ == '__main__':
    start_http_server(9090)
    while True:
        collect_metrics()
        time.sleep(15)
```

### Grafana Dashboard

Key panels for a Grafana dashboard:

1. **Active Connections** (time series)
   - Query: `playwright_active_connections_total`

2. **Worker Count** (stat)
   - Query: `sum(playwright_worker_count)`

3. **Utilization %** (gauge)
   - Query: `playwright_active_connections_total / (sum(playwright_worker_count) * 5) * 100`

4. **Lifetime Distribution** (bar chart)
   - Query: `playwright_worker_lifetime`

5. **Connection Rate** (time series)
   - Query: `rate(playwright_connections_total[5m])`

---

## Alerting

### Alert Rules (Prometheus)

```yaml
groups:
  - name: playwright
    rules:
      - alert: PlaywrightNoWorkers
        expr: sum(playwright_worker_count) == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "No Playwright workers registered"

      - alert: PlaywrightHighUtilization
        expr: playwright_active_connections_total / (sum(playwright_worker_count) * 5) > 0.9
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Playwright cluster at >90% capacity"

      - alert: PlaywrightWorkersDeclining
        expr: sum(playwright_worker_count) < sum(playwright_worker_count offset 10m)
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Playwright worker count is declining"
```

### Simple Alerting Script

```bash
#!/bin/bash
# alert-check.sh

WEBHOOK_URL="https://hooks.slack.com/services/..."

check_workers() {
    WORKERS=$(docker compose exec -T redis redis-cli KEYS "worker:*:*" | wc -l)
    if [ "$WORKERS" -lt 2 ]; then
        curl -X POST -H 'Content-type: application/json' \
            --data '{"text":"⚠️ Playwright: Only '$WORKERS' workers registered!"}' \
            "$WEBHOOK_URL"
    fi
}

check_utilization() {
    ACTIVE=$(curl -s http://localhost:8080/metrics | jq -r '.activeConnections')
    WORKERS=$(docker compose exec -T redis redis-cli KEYS "worker:*:*" | wc -l)
    CAPACITY=$((WORKERS * 5))

    if [ "$ACTIVE" -gt $((CAPACITY * 9 / 10)) ]; then
        curl -X POST -H 'Content-type: application/json' \
            --data '{"text":"⚠️ Playwright: Utilization at '$ACTIVE'/'$CAPACITY' (>90%)"}' \
            "$WEBHOOK_URL"
    fi
}

check_workers
check_utilization
```

Run via cron every minute.

---

## Container Monitoring

### Docker Stats

```bash
docker stats --format "table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}"
```

### Kubernetes Metrics

```bash
# Pod resource usage
kubectl top pods -n playwright

# Node resource usage
kubectl top nodes
```

---

## Redis Monitoring

### Key Metrics

```bash
# Memory usage
docker compose exec redis redis-cli INFO memory | grep used_memory_human

# Connected clients
docker compose exec redis redis-cli INFO clients | grep connected_clients

# Operations per second
docker compose exec redis redis-cli INFO stats | grep instantaneous_ops_per_sec

# Latency
docker compose exec redis redis-cli --latency
```

### Redis Slowlog

```bash
# Check slow commands
docker compose exec redis redis-cli SLOWLOG GET 10
```

---

## Checklist for Production Monitoring

```
□ Metrics collection configured
  - Proxy /metrics endpoint scraped
  - Redis metrics collected
  - Container metrics collected

□ Dashboards created
  - Active connections over time
  - Worker count and distribution
  - Utilization percentage
  - Error rates

□ Alerts configured
  - No workers available
  - High utilization (>90%)
  - Worker count declining
  - High error rate

□ Log aggregation set up
  - Proxy logs collected
  - Worker logs collected
  - Searchable and filterable

□ Runbooks documented
  - What to do for each alert
  - Escalation procedures
  - Contact information
```

---

## Next Steps

- **[Troubleshooting](troubleshooting.md)** — When alerts fire
- **[Maintenance](maintenance.md)** — Routine operations
