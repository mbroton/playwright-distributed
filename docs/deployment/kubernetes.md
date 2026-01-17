# Kubernetes Deployment

Kubernetes provides auto-scaling, high availability, and advanced resource management. This guide covers deploying playwright-distributed on Kubernetes.

---

## Prerequisites

- Kubernetes cluster (1.20+)
- `kubectl` configured
- Helm (optional, for Redis)

---

## Quick Start

```bash
# Create namespace
kubectl create namespace playwright

# Deploy Redis
kubectl apply -f redis.yaml -n playwright

# Deploy Proxy
kubectl apply -f proxy.yaml -n playwright

# Deploy Workers
kubectl apply -f worker.yaml -n playwright

# Verify
kubectl get pods -n playwright
```

---

## Manifests

### Redis

```yaml
# redis.yaml
apiVersion: v1
kind: Service
metadata:
  name: redis
spec:
  selector:
    app: redis
  ports:
    - port: 6379
      targetPort: 6379

---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
        - name: redis
          image: redis:8.0
          ports:
            - containerPort: 6379
          resources:
            requests:
              memory: "128Mi"
              cpu: "100m"
            limits:
              memory: "512Mi"
              cpu: "500m"
```

For production, consider using a Redis Helm chart or managed Redis (AWS ElastiCache, GCP Memorystore).

### Proxy

```yaml
# proxy.yaml
apiVersion: v1
kind: Service
metadata:
  name: playwright-proxy
spec:
  type: LoadBalancer  # Or ClusterIP with Ingress
  selector:
    app: playwright-proxy
  ports:
    - port: 8080
      targetPort: 8080

---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: playwright-proxy
spec:
  replicas: 2
  selector:
    matchLabels:
      app: playwright-proxy
  template:
    metadata:
      labels:
        app: playwright-proxy
    spec:
      containers:
        - name: proxy
          image: ghcr.io/mbroton/playwright-distributed-proxy:latest
          ports:
            - containerPort: 8080
          env:
            - name: REDIS_HOST
              value: "redis"
            - name: REDIS_PORT
              value: "6379"
            - name: MAX_CONCURRENT_SESSIONS
              value: "10"
            - name: MAX_LIFETIME_SESSIONS
              value: "100"
            - name: LOG_LEVEL
              value: "info"
            - name: LOG_FORMAT
              value: "json"
          resources:
            requests:
              memory: "256Mi"
              cpu: "250m"
            limits:
              memory: "512Mi"
              cpu: "1000m"
          livenessProbe:
            httpGet:
              path: /metrics
              port: 8080
            initialDelaySeconds: 10
            periodSeconds: 30
          readinessProbe:
            httpGet:
              path: /metrics
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
```

### Workers

```yaml
# worker.yaml
apiVersion: v1
kind: Service
metadata:
  name: playwright-worker
spec:
  clusterIP: None  # Headless for direct pod access
  selector:
    app: playwright-worker
  ports:
    - port: 3131
      targetPort: 3131

---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: playwright-worker
spec:
  replicas: 3
  selector:
    matchLabels:
      app: playwright-worker
  template:
    metadata:
      labels:
        app: playwright-worker
    spec:
      containers:
        - name: worker
          image: ghcr.io/mbroton/playwright-distributed-worker:latest
          ports:
            - containerPort: 3131
          env:
            - name: REDIS_URL
              value: "redis://redis:6379"
            - name: PORT
              value: "3131"
            - name: PRIVATE_HOSTNAME
              value: "playwright-worker"  # Service name
            - name: BROWSER_TYPE
              value: "chromium"
            - name: HEADLESS
              value: "true"
            - name: HEARTBEAT_INTERVAL
              value: "10"
            - name: REDIS_KEY_TTL
              value: "30"
            - name: LOG_LEVEL
              value: "info"
            - name: LOG_FORMAT
              value: "json"
          resources:
            requests:
              memory: "1Gi"
              cpu: "500m"
            limits:
              memory: "2Gi"
              cpu: "2000m"
```

---

## Multi-Browser Setup

Deploy separate worker deployments for each browser:

```yaml
# worker-chromium.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: playwright-worker-chromium
spec:
  replicas: 3
  selector:
    matchLabels:
      app: playwright-worker
      browser: chromium
  template:
    metadata:
      labels:
        app: playwright-worker
        browser: chromium
    spec:
      containers:
        - name: worker
          image: ghcr.io/mbroton/playwright-distributed-worker:latest
          env:
            - name: BROWSER_TYPE
              value: "chromium"
            - name: PRIVATE_HOSTNAME
              value: "playwright-worker"
            # ... other env vars

---
# worker-firefox.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: playwright-worker-firefox
spec:
  replicas: 2
  selector:
    matchLabels:
      app: playwright-worker
      browser: firefox
  template:
    # ... similar to chromium but with BROWSER_TYPE=firefox
```

---

## Auto-Scaling (HPA)

Scale workers based on CPU usage:

```yaml
# worker-hpa.yaml
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
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
    - type: Resource
      resource:
        name: memory
        target:
          type: Utilization
          averageUtilization: 80
```

For better scaling based on active connections, consider custom metrics.

---

## Ingress with TLS

```yaml
# ingress.yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: playwright-ingress
  annotations:
    kubernetes.io/ingress.class: nginx
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-read-timeout: "86400"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "86400"
    nginx.ingress.kubernetes.io/upstream-hash-by: "$remote_addr"
spec:
  tls:
    - hosts:
        - grid.example.com
      secretName: playwright-tls
  rules:
    - host: grid.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: playwright-proxy
                port:
                  number: 8080
```

**Important WebSocket annotations**:
- `proxy-read-timeout` and `proxy-send-timeout`: Long timeouts for WebSocket
- `upstream-hash-by`: Sticky sessions (optional)

---

## Network Policies

Restrict network access:

```yaml
# network-policy.yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: playwright-network-policy
spec:
  podSelector:
    matchLabels:
      app: playwright-worker
  policyTypes:
    - Ingress
    - Egress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: playwright-proxy
      ports:
        - port: 3131
  egress:
    - to:
        - podSelector:
            matchLabels:
              app: redis
      ports:
        - port: 6379
    - to:  # Allow outbound internet for browser
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - 10.0.0.0/8
              - 172.16.0.0/12
              - 192.168.0.0/16
```

---

## ConfigMap for Shared Config

```yaml
# configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: playwright-config
data:
  MAX_CONCURRENT_SESSIONS: "10"
  MAX_LIFETIME_SESSIONS: "100"
  HEARTBEAT_INTERVAL: "10"
  REDIS_KEY_TTL: "30"
  LOG_LEVEL: "info"
  LOG_FORMAT: "json"
```

Reference in deployments:

```yaml
containers:
  - name: worker
    envFrom:
      - configMapRef:
          name: playwright-config
    env:
      - name: BROWSER_TYPE
        value: "chromium"
```

---

## Pod Disruption Budget

Ensure availability during updates:

```yaml
# pdb.yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: playwright-worker-pdb
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app: playwright-worker
```

---

## Resource Recommendations

### Development/Testing

```yaml
resources:
  requests:
    memory: "512Mi"
    cpu: "250m"
  limits:
    memory: "1Gi"
    cpu: "1000m"
```

### Production

```yaml
resources:
  requests:
    memory: "1Gi"
    cpu: "500m"
  limits:
    memory: "2Gi"
    cpu: "2000m"
```

### High-Memory Workloads

```yaml
resources:
  requests:
    memory: "2Gi"
    cpu: "1000m"
  limits:
    memory: "4Gi"
    cpu: "4000m"
```

---

## Helm Chart (Future)

A Helm chart is planned for easier deployment. For now, use the manifests above.

---

## Monitoring with Prometheus

Add annotations for Prometheus scraping:

```yaml
template:
  metadata:
    labels:
      app: playwright-proxy
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "8080"
      prometheus.io/path: "/metrics"
```

---

## Troubleshooting

### Pods Pending

```bash
kubectl describe pod <pod-name> -n playwright
```

Common causes:
- Insufficient resources (check resource requests)
- No nodes available
- PersistentVolume not bound

### Workers Not Registering

```bash
# Check worker logs
kubectl logs -l app=playwright-worker -n playwright

# Check Redis connectivity
kubectl exec -it <worker-pod> -n playwright -- nc -zv redis 6379
```

### Connection Timeouts

Check Ingress configuration:
- WebSocket timeouts must be long (86400s)
- Ensure WebSocket upgrade is allowed

---

## Commands Reference

| Task | Command |
|------|---------|
| Deploy | `kubectl apply -f . -n playwright` |
| Scale | `kubectl scale deployment playwright-worker --replicas=5 -n playwright` |
| View pods | `kubectl get pods -n playwright` |
| View logs | `kubectl logs -f deployment/playwright-worker -n playwright` |
| Describe | `kubectl describe pod <name> -n playwright` |
| Delete | `kubectl delete -f . -n playwright` |

---

## Next Steps

- **[Scaling Strategies](scaling.md)** — Plan capacity
- **[Monitoring](../operations/monitoring.md)** — Set up observability
- **[Troubleshooting](../operations/troubleshooting.md)** — Debug issues
