# Deployment Guide

playwright-distributed can be deployed anywhere you can run containers. This guide covers common deployment patterns.

---

## Deployment Options

### [Docker Compose](docker-compose.md)

Best for:
- Local development
- Small teams
- Single-server deployments
- Quick prototyping

```bash
docker compose up -d
```

### [Kubernetes](kubernetes.md)

Best for:
- Production deployments
- Auto-scaling requirements
- High availability needs
- Multi-node clusters

```bash
kubectl apply -f playwright-distributed/
```

### [Scaling Strategies](scaling.md)

Learn when and how to scale:
- Horizontal vs vertical scaling
- Worker sizing guidelines
- Load balancing considerations

---

## Architecture Recap

Before deploying, understand the components:

```
┌─────────────────────────────────────────────────────────────────┐
│                     Deployment Architecture                      │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│   CLIENTS (Your Applications)                                    │
│       │                                                          │
│       │ WebSocket (exposed)                                      │
│       ▼                                                          │
│   ┌─────────┐                                                    │
│   │  PROXY  │  Stateless, can run multiple instances            │
│   └────┬────┘                                                    │
│        │                                                         │
│        ├──────────────────────┐                                  │
│        │ WebSocket (private)  │ Redis (private)                  │
│        ▼                      ▼                                  │
│   ┌─────────┐           ┌─────────┐                              │
│   │ WORKERS │           │  REDIS  │                              │
│   └─────────┘           └─────────┘                              │
│   Stateless,            Single instance                          │
│   run as many           (or cluster for HA)                      │
│   as needed                                                      │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

**Key points**:
- Only the proxy needs public exposure
- Workers and Redis are internal
- Proxy and workers are stateless (Redis holds state)
- All components need network connectivity to Redis

---

## Quick Comparison

| Aspect | Docker Compose | Kubernetes |
|--------|----------------|------------|
| Complexity | Low | Medium-High |
| Scaling | Manual | Auto-scaling |
| High Availability | Limited | Built-in |
| Resource Management | Basic | Advanced |
| Best For | Dev/Small prod | Large production |

---

## Resource Requirements

### Minimum (Development)

| Component | CPU | Memory |
|-----------|-----|--------|
| Proxy | 0.5 core | 256 MB |
| Worker | 1 core | 1 GB |
| Redis | 0.25 core | 128 MB |

### Recommended (Production)

| Component | CPU | Memory | Instances |
|-----------|-----|--------|-----------|
| Proxy | 1 core | 512 MB | 2+ |
| Worker | 2 cores | 2 GB | 3+ |
| Redis | 1 core | 512 MB | 1 (or cluster) |

### Worker Memory Formula

```
Memory per worker ≈ Browser baseline + (Contexts × Context size)
                 ≈ 300 MB + (MAX_CONCURRENT_SESSIONS × 150 MB)
```

For `MAX_CONCURRENT_SESSIONS=5`:
```
≈ 300 MB + (5 × 150 MB) = 1050 MB ≈ 1 GB
```

Add buffer for spikes: **1.5-2 GB recommended** per worker.

---

## Pre-Deployment Checklist

Before deploying to production:

```
□ Network connectivity verified (proxy → workers, all → Redis)
□ PRIVATE_HOSTNAME set correctly for your environment
□ Timing configuration reviewed (see Timing Configuration)
□ Resource limits set on containers
□ Restart policies configured
□ Logging configured for aggregation
□ Monitoring endpoints accessible
□ Backup strategy for Redis (if persistent)
```

---

## Security Checklist

```
□ Only proxy port exposed publicly
□ TLS termination configured (if needed)
□ Redis not accessible from public network
□ Workers not accessible from public network
□ Network policies in place (Kubernetes)
□ No sensitive data in environment variables logged
```

---

## Next Steps

Choose your deployment method:

- **[Docker Compose](docker-compose.md)** — Start here for simplicity
- **[Kubernetes](kubernetes.md)** — For production-grade deployments
- **[Scaling Strategies](scaling.md)** — Plan your capacity
