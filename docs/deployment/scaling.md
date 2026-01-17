# Scaling Strategies

This guide helps you plan and execute scaling for playwright-distributed.

---

## Understanding Capacity

### Capacity Formula

```
Total Concurrent Capacity = Workers × MAX_CONCURRENT_SESSIONS
```

**Example**: 5 workers × 10 concurrent sessions = 50 concurrent connections

### Throughput Formula

```
Throughput (sessions/hour) = (Workers × MAX_LIFETIME_SESSIONS) / Restart_Time
```

With quick restarts (~10 seconds):
- 5 workers × 100 lifetime × 360 (restarts/hour) = 180,000 sessions/hour theoretical max

In practice, factor in actual session duration and don't run at 100% capacity.

---

## When to Scale

### Scale Up When

| Signal | Action |
|--------|--------|
| 503 errors increasing | Add workers |
| Active connections near capacity | Add workers |
| Response times increasing | Add workers or increase resources |
| Worker CPU > 80% sustained | Add resources or workers |
| Worker memory near limit | Add resources or reduce concurrent sessions |

### Scale Down When

| Signal | Action |
|--------|--------|
| Workers consistently idle | Remove workers |
| Active connections < 50% capacity | Remove workers |
| Cost exceeds value | Remove workers |

---

## Horizontal vs Vertical Scaling

### Horizontal (Add Workers)

**Pros**:
- Linear capacity increase
- Better fault tolerance
- Easy with orchestration

**Cons**:
- More containers to manage
- More network connections
- Higher Redis load

**When to use**:
- Need more concurrent capacity
- Want high availability
- Have variable load

### Vertical (Bigger Workers)

**Pros**:
- Fewer containers
- Simpler management
- Less network overhead

**Cons**:
- Upper limit on single-machine resources
- Single point of failure per worker
- Diminishing returns

**When to use**:
- Workers are memory-constrained
- Have stable, predictable load
- Cost-optimizing for specific workload

---

## Worker Sizing

### Memory Guidelines

| MAX_CONCURRENT_SESSIONS | Recommended Memory |
|------------------------|-------------------|
| 3 | 1 GB |
| 5 | 1.5 GB |
| 10 | 2.5 GB |
| 15 | 4 GB |
| 20 | 5 GB |

These are estimates. Browser memory usage varies by:
- Page complexity (JS, images, DOM size)
- Number of tabs/contexts
- Browser type (Firefox tends to use more)

### CPU Guidelines

| Concurrent Sessions | Recommended CPU |
|--------------------|-----------------|
| 1-5 | 1 core |
| 5-10 | 2 cores |
| 10-20 | 4 cores |

CPU usage spikes during page loads and JavaScript execution.

---

## Scaling Patterns

### Pattern 1: Start Small, Scale Up

```
Phase 1 (MVP):
├── 1 proxy
├── 2 workers (MAX_CONCURRENT=5)
└── Capacity: 10 concurrent

Phase 2 (Growth):
├── 2 proxies (HA)
├── 5 workers (MAX_CONCURRENT=10)
└── Capacity: 50 concurrent

Phase 3 (Scale):
├── 2 proxies
├── 20 workers (MAX_CONCURRENT=10)
└── Capacity: 200 concurrent
```

### Pattern 2: Multi-Browser Split

```
Total: 100 concurrent capacity

Chromium: 60% of traffic
├── 8 workers × 8 concurrent = 64 slots

Firefox: 30% of traffic
├── 4 workers × 8 concurrent = 32 slots

WebKit: 10% of traffic
├── 2 workers × 5 concurrent = 10 slots
```

Scale each browser type independently based on usage.

### Pattern 3: Time-Based Scaling

```
Business Hours (9am-6pm):
├── 10 workers

Off-Hours (6pm-9am):
├── 3 workers

Weekends:
├── 2 workers
```

Implement with Kubernetes CronJobs or cloud auto-scaling schedules.

---

## Auto-Scaling

### Kubernetes HPA

Scale based on CPU:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: playwright-worker-hpa
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: playwright-worker
  minReplicas: 2
  maxReplicas: 20
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 300  # Wait 5 min before scaling down
    scaleUp:
      stabilizationWindowSeconds: 60   # Scale up quickly
```

### Custom Metrics Scaling

For better scaling, use custom metrics based on active connections:

```yaml
metrics:
  - type: External
    external:
      metric:
        name: playwright_active_connections
      target:
        type: AverageValue
        averageValue: "8"  # Target 8 connections per worker
```

This requires a Prometheus adapter or similar.

### AWS Auto Scaling

For EC2/ECS, scale based on CloudWatch metrics:

```
Scale Up:
  Metric: Average active connections > 80% capacity
  Action: Add 2 workers
  Cooldown: 5 minutes

Scale Down:
  Metric: Average active connections < 30% capacity
  Action: Remove 1 worker
  Cooldown: 10 minutes
```

---

## Scaling the Proxy

### When to Scale Proxy

The proxy is lightweight. Scale when:
- Need high availability (run 2+)
- Single proxy CPU is saturated
- Geographic distribution needed

### How to Scale Proxy

Proxies are stateless—just add more behind a load balancer:

```
                 Load Balancer
                      │
        ┌─────────────┼─────────────┐
        │             │             │
        ▼             ▼             ▼
    Proxy 1       Proxy 2       Proxy 3
        │             │             │
        └─────────────┼─────────────┘
                      │
                    Redis
```

All proxies share state through Redis.

---

## Scaling Redis

### When to Scale Redis

Redis is rarely the bottleneck. Scale when:
- Thousands of workers
- Very high connection churn
- Complex multi-datacenter setup

### How to Scale Redis

**Single Instance** (most cases):
- Sufficient for 100+ workers
- Use persistence for durability

**Redis Sentinel** (HA):
- Automatic failover
- No sharding, same capacity

**Redis Cluster** (scale):
- Horizontal scaling
- More complex setup
- For very large deployments

---

## Cost Optimization

### Right-Size Workers

```
Over-provisioned:
├── 10 workers × 2GB = 20GB memory
├── Average utilization: 30%
└── Wasted: 14GB

Right-sized:
├── 5 workers × 3GB = 15GB memory
├── Average utilization: 60%
└── Wasted: 6GB
```

### Use Spot/Preemptible Instances

Workers are stateless and restart frequently anyway:

```yaml
# Kubernetes
spec:
  nodeSelector:
    cloud.google.com/gke-preemptible: "true"
```

```
# AWS
capacity_type = "SPOT"
```

Savings: 60-80% on compute costs.

### Time-Based Scaling

Don't run full capacity 24/7 if load is time-based:

```
Peak (8 hours): 20 workers
Off-peak (16 hours): 5 workers

Without scaling: 20 workers × 24 hours = 480 worker-hours
With scaling: (20 × 8) + (5 × 16) = 240 worker-hours
Savings: 50%
```

---

## Capacity Planning Checklist

```
□ Measure current usage patterns
  - Peak concurrent connections
  - Average session duration
  - Traffic distribution by time

□ Define SLOs
  - Target availability (99.9%?)
  - Acceptable queue time
  - Error rate threshold

□ Calculate capacity needs
  - Peak + 30% buffer
  - Account for worker restarts
  - Consider burst capacity

□ Plan scaling triggers
  - CPU/memory thresholds
  - Connection count thresholds
  - Error rate thresholds

□ Test scaling behavior
  - Scale up response time
  - Scale down behavior
  - Failure scenarios
```

---

## Example: Planning for 100 Concurrent Users

**Requirements**:
- 100 peak concurrent connections
- 99% availability during restarts
- Each session lasts ~30 seconds

**Calculation**:

```
Base capacity: 100 concurrent
Buffer (30%): 30 concurrent
Target: 130 concurrent slots

With MAX_CONCURRENT_SESSIONS=10:
Workers needed: 130 / 10 = 13 workers

For 99% availability during 1 worker restart:
Available: 12 / 13 = 92% ← Not enough

Adjusted:
Workers: 15
Available during restart: 14 / 15 = 93%
With 2 workers restarting: 13 / 15 = 87% ← Still tight

Final:
Workers: 20
Capacity: 200 concurrent
Available during 2 restarts: 180 (90%)
Buffer for spikes: 80 slots (80%)
```

**Result**: Deploy 20 workers with `MAX_CONCURRENT_SESSIONS=10` for 100 concurrent users with good margin.

---

## Next Steps

- **[Monitoring](../operations/monitoring.md)** — Track scaling metrics
- **[Kubernetes](kubernetes.md)** — Auto-scaling setup
- **[Docker Compose](docker-compose.md)** — Manual scaling
