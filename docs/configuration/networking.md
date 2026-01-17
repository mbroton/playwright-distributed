# Networking Configuration

Networking is the most common source of issues in playwright-distributed. This guide explains the communication patterns and how to configure them correctly.

---

## The Three Network Paths

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Network Topology                             │
├─────────────────────────────────────────────────────────────────────┤
│                                                                      │
│   CLIENT ──────────────► PROXY ──────────────► WORKER               │
│           Path 1                   Path 2                            │
│           (Public)                 (Private)                         │
│                            │                      │                  │
│                            │                      │                  │
│                            └────────┬─────────────┘                  │
│                                     │                                │
│                                     ▼                                │
│                                   REDIS                              │
│                                  Path 3                              │
│                                 (Private)                            │
│                                                                      │
└─────────────────────────────────────────────────────────────────────┘
```

### Path 1: Client → Proxy (Public)

This is the only path that should be publicly accessible.

- **Protocol**: WebSocket
- **Default Port**: 8080
- **Exposure**: Public (with optional TLS termination)

### Path 2: Proxy → Worker (Private)

The proxy connects to workers to relay messages.

- **Protocol**: WebSocket
- **Default Port**: 3131
- **Exposure**: Private network only

### Path 3: Proxy/Worker → Redis (Private)

Both proxy and workers need Redis access.

- **Protocol**: TCP (Redis protocol)
- **Default Port**: 6379
- **Exposure**: Private network only

---

## The PRIVATE_HOSTNAME Problem

This is the #1 networking issue.

### What Happens

1. Worker starts and launches browser on `PORT` (e.g., 3131)
2. Worker registers with Redis: "Connect to me at `ws://PRIVATE_HOSTNAME:PORT/...`"
3. Proxy reads this address from Redis
4. Proxy tries to connect to that address

### The Problem

The `PRIVATE_HOSTNAME` must be resolvable **from the proxy**, not from the worker.

```
❌ Common mistake:

Worker thinks its hostname is "abc123" (container ID)
Worker registers: ws://abc123:3131/...
Proxy tries to connect to abc123
Proxy can't resolve "abc123"
Connection fails
```

### The Solution

Explicitly set `PRIVATE_HOSTNAME` to something the proxy can resolve:

```yaml
services:
  worker:
    environment:
      - PRIVATE_HOSTNAME=worker  # Docker service name
```

---

## Docker Compose Networking

Docker Compose creates a default network where services can reach each other by name.

### Working Configuration

```yaml
version: "3.8"

services:
  redis:
    image: redis:8.0
    # No ports exposed - internal only

  proxy:
    image: ghcr.io/mbroton/playwright-distributed-proxy:latest
    ports:
      - "8080:8080"  # Only exposed port
    environment:
      - REDIS_HOST=redis  # Service name
      - REDIS_PORT=6379

  worker:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
    # No ports exposed - internal only
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker  # Service name
```

### How It Works

1. Docker Compose creates network `playwright-distributed_default`
2. Services can reach each other by service name
3. `redis` resolves to the Redis container
4. `worker` resolves to the worker container

### With Multiple Workers

```yaml
services:
  worker-chromium:
    environment:
      - PRIVATE_HOSTNAME=worker-chromium

  worker-firefox:
    environment:
      - PRIVATE_HOSTNAME=worker-firefox
```

Each worker registers with its service name.

### Scaling Workers

```bash
docker compose up -d --scale worker=3
```

With scaling, Docker names containers `worker-1`, `worker-2`, etc. Since `PRIVATE_HOSTNAME=worker` is set, all register as "worker" which Docker load-balances.

For better control, use explicit service names or Kubernetes.

---

## Kubernetes Networking

Kubernetes offers more networking options.

### Option 1: Service DNS (Recommended)

```yaml
apiVersion: v1
kind: Service
metadata:
  name: playwright-worker
spec:
  selector:
    app: playwright-worker
  ports:
    - port: 3131

---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: playwright-worker
spec:
  template:
    spec:
      containers:
        - name: worker
          env:
            - name: PRIVATE_HOSTNAME
              value: "playwright-worker"  # Service name
            - name: PORT
              value: "3131"
```

All workers register as `playwright-worker:3131`. Kubernetes service does the load balancing.

### Option 2: Headless Service (Direct Pod Access)

For more control, use a headless service:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: playwright-worker-headless
spec:
  clusterIP: None  # Headless
  selector:
    app: playwright-worker
  ports:
    - port: 3131

---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: playwright-worker
spec:
  serviceName: playwright-worker-headless
  template:
    spec:
      containers:
        - name: worker
          env:
            - name: PRIVATE_HOSTNAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: PORT
              value: "3131"
```

Each pod registers with its own name (e.g., `playwright-worker-0:3131`).

### Redis in Kubernetes

```yaml
apiVersion: v1
kind: Service
metadata:
  name: redis
spec:
  selector:
    app: redis
  ports:
    - port: 6379

---
# Worker config
env:
  - name: REDIS_URL
    value: "redis://redis:6379"
```

---

## Bare Metal / VMs

Without container orchestration, use IP addresses or DNS.

### Using IP Addresses

```bash
# On worker machine (10.0.1.50)
export PRIVATE_HOSTNAME=10.0.1.50
export PORT=3131
export REDIS_URL=redis://10.0.1.100:6379
./worker
```

```bash
# On proxy machine (10.0.1.10)
export REDIS_HOST=10.0.1.100
export REDIS_PORT=6379
./proxy
```

### Using DNS

If you have internal DNS:

```bash
# On worker machine
export PRIVATE_HOSTNAME=worker1.internal.example.com
```

Ensure the proxy can resolve `worker1.internal.example.com`.

---

## Verifying Connectivity

### Step 1: Check Redis Connection

From the proxy container:
```bash
docker compose exec proxy sh -c "nc -zv redis 6379"
```

From the worker container:
```bash
docker compose exec worker sh -c "nc -zv redis 6379"
```

### Step 2: Check Worker Registration

```bash
docker compose exec redis redis-cli KEYS "worker:*"
```

Should return registered workers.

### Step 3: Check Registered Endpoint

```bash
docker compose exec redis redis-cli HGET worker:chromium:abc123 wsEndpoint
```

Returns something like `ws://worker:3131/playwright/chromium/...`

### Step 4: Test Proxy → Worker

From the proxy container, try to reach the worker:
```bash
docker compose exec proxy wget -q -O- --timeout=2 http://worker:3131 || echo "Can't connect"
```

### Step 5: End-to-End Test

```python
from playwright.async_api import async_playwright

async with async_playwright() as p:
    browser = await p.chromium.connect("ws://localhost:8080", timeout=5000)
    print("Connected!")
    await browser.close()
```

---

## Common Issues

### "No Available Servers" but Workers Are Running

**Cause**: Proxy can't reach workers at their registered address.

**Debug**:
```bash
# Get registered endpoint
docker compose exec redis redis-cli HGET worker:chromium:abc123 wsEndpoint

# Try to reach it from proxy
docker compose exec proxy wget -q -O- --timeout=2 <endpoint>
```

**Fix**: Set `PRIVATE_HOSTNAME` to something resolvable from the proxy.

### Connection Timeout

**Cause**: Network path is blocked or slow.

**Debug**:
```bash
# Check if port is reachable
docker compose exec proxy sh -c "nc -zv worker 3131"
```

**Fix**: Check firewall rules, network policies, or security groups.

### Works Locally, Fails in Production

**Cause**: Different network topology.

**Debug**:
- Verify `PRIVATE_HOSTNAME` is appropriate for production environment
- Check that all services are on the same network/VPC
- Verify DNS resolution works

---

## Security Recommendations

### 1. Only Expose the Proxy

```yaml
services:
  proxy:
    ports:
      - "8080:8080"  # Only this

  worker:
    # No ports section

  redis:
    # No ports section
```

### 2. Use Private Networks

In cloud environments, put workers and Redis on private subnets.

### 3. TLS Termination

Terminate TLS at a load balancer in front of the proxy:

```
Client ──[HTTPS/WSS]──► Load Balancer ──[HTTP/WS]──► Proxy
```

### 4. Network Policies (Kubernetes)

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: worker-policy
spec:
  podSelector:
    matchLabels:
      app: playwright-worker
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: playwright-proxy
      ports:
        - port: 3131
```

Only allow the proxy to reach workers.

---

## See Also

- **[Troubleshooting](../operations/troubleshooting.md)** — Network issue diagnosis
- **[Docker Compose Deployment](../deployment/docker-compose.md)** — Complete Docker setup
- **[Kubernetes Deployment](../deployment/kubernetes.md)** — Complete K8s setup
