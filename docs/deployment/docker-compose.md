# Docker Compose Deployment

Docker Compose is the simplest way to deploy playwright-distributed. This guide covers development and production configurations.

---

## Quick Start

```bash
# Clone and start
git clone https://github.com/mbroton/playwright-distributed.git
cd playwright-distributed
docker compose up -d

# Verify
docker compose ps
curl http://localhost:8080/metrics
```

---

## Development Configuration

For local development with all browser types:

```yaml
# docker-compose.local.yaml
version: "3.8"

services:
  redis:
    image: redis:8.0
    restart: unless-stopped

  proxy:
    build:
      context: ./proxy
    ports:
      - "8080:8080"
    environment:
      - REDIS_HOST=redis
      - REDIS_PORT=6379
      - LOG_LEVEL=debug
      - LOG_FORMAT=text
    depends_on:
      - redis
    restart: unless-stopped

  worker-chromium:
    build:
      context: ./worker
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker-chromium
      - BROWSER_TYPE=chromium
      - LOG_LEVEL=debug
      - LOG_FORMAT=text
    depends_on:
      - redis
    restart: unless-stopped

  worker-firefox:
    build:
      context: ./worker
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker-firefox
      - BROWSER_TYPE=firefox
      - LOG_LEVEL=debug
      - LOG_FORMAT=text
    depends_on:
      - redis
    restart: unless-stopped

  worker-webkit:
    build:
      context: ./worker
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker-webkit
      - BROWSER_TYPE=webkit
      - LOG_LEVEL=debug
      - LOG_FORMAT=text
    depends_on:
      - redis
    restart: unless-stopped
```

Run with:
```bash
docker compose -f docker-compose.local.yaml up -d
```

---

## Production Configuration

For production with a single browser type and proper settings:

```yaml
# docker-compose.prod.yaml
version: "3.8"

services:
  redis:
    image: redis:8.0
    restart: always
    volumes:
      - redis-data:/data
    command: redis-server --appendonly yes
    deploy:
      resources:
        limits:
          memory: 512M

  proxy:
    image: ghcr.io/mbroton/playwright-distributed-proxy:latest
    ports:
      - "8080:8080"
    environment:
      - REDIS_HOST=redis
      - REDIS_PORT=6379
      - MAX_CONCURRENT_SESSIONS=10
      - MAX_LIFETIME_SESSIONS=100
      - LOG_LEVEL=info
      - LOG_FORMAT=json
    depends_on:
      - redis
    restart: always
    deploy:
      resources:
        limits:
          memory: 512M
          cpus: '1.0'

  worker:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
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
    depends_on:
      - redis
    restart: always
    deploy:
      resources:
        limits:
          memory: 2G
          cpus: '2.0'
        reservations:
          memory: 512M

volumes:
  redis-data:
```

---

## Scaling Workers

### Manual Scaling

Scale to N workers:

```bash
docker compose -f docker-compose.prod.yaml up -d --scale worker=5
```

Check running instances:

```bash
docker compose ps
```

### Dynamic Scaling

Scale up during high load:
```bash
docker compose up -d --scale worker=10
```

Scale down to save resources:
```bash
docker compose up -d --scale worker=3
```

---

## Multi-Browser Production

If you need multiple browser types in production:

```yaml
version: "3.8"

services:
  redis:
    image: redis:8.0
    restart: always

  proxy:
    image: ghcr.io/mbroton/playwright-distributed-proxy:latest
    ports:
      - "8080:8080"
    environment:
      - REDIS_HOST=redis
      - REDIS_PORT=6379
    restart: always

  worker-chromium:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker-chromium
      - BROWSER_TYPE=chromium
    restart: always
    deploy:
      replicas: 3  # Docker Compose 3.x with swarm mode

  worker-firefox:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker-firefox
      - BROWSER_TYPE=firefox
    restart: always

  worker-webkit:
    image: ghcr.io/mbroton/playwright-distributed-worker:latest
    environment:
      - REDIS_URL=redis://redis:6379
      - PORT=3131
      - PRIVATE_HOSTNAME=worker-webkit
      - BROWSER_TYPE=webkit
    restart: always
```

---

## Health Checks

Add health checks for better container management:

```yaml
services:
  proxy:
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:8080/metrics"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 10s

  redis:
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
```

---

## Logging

### View Logs

```bash
# All services
docker compose logs -f

# Specific service
docker compose logs -f proxy
docker compose logs -f worker

# Last N lines
docker compose logs --tail=100 proxy
```

### Log Aggregation

For production, send logs to an aggregator:

```yaml
services:
  proxy:
    logging:
      driver: "json-file"
      options:
        max-size: "10m"
        max-file: "3"
```

Or use a logging driver like `fluentd` or `gelf`:

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

## Persistence

Redis data persistence for crash recovery:

```yaml
services:
  redis:
    volumes:
      - redis-data:/data
    command: redis-server --appendonly yes

volumes:
  redis-data:
```

**Note**: For playwright-distributed, Redis persistence is optional. If Redis loses data, workers re-register on their next heartbeat (within seconds).

---

## Reverse Proxy / TLS

Put a reverse proxy in front for TLS termination:

### With Traefik

```yaml
services:
  traefik:
    image: traefik:v2.10
    command:
      - "--providers.docker=true"
      - "--entrypoints.websecure.address=:443"
      - "--certificatesresolvers.letsencrypt.acme.tlschallenge=true"
      - "--certificatesresolvers.letsencrypt.acme.email=you@example.com"
      - "--certificatesresolvers.letsencrypt.acme.storage=/letsencrypt/acme.json"
    ports:
      - "443:443"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - letsencrypt:/letsencrypt

  proxy:
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.playwright.rule=Host(`grid.example.com`)"
      - "traefik.http.routers.playwright.entrypoints=websecure"
      - "traefik.http.routers.playwright.tls.certresolver=letsencrypt"
      - "traefik.http.services.playwright.loadbalancer.server.port=8080"
```

### With Nginx

```yaml
services:
  nginx:
    image: nginx:alpine
    ports:
      - "443:443"
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
      - ./certs:/etc/nginx/certs:ro
```

```nginx
# nginx.conf
upstream playwright {
    server proxy:8080;
}

server {
    listen 443 ssl;
    server_name grid.example.com;

    ssl_certificate /etc/nginx/certs/cert.pem;
    ssl_certificate_key /etc/nginx/certs/key.pem;

    location / {
        proxy_pass http://playwright;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_read_timeout 86400;
    }
}
```

---

## Troubleshooting

### Containers Keep Restarting

Check logs:
```bash
docker compose logs worker
```

Common causes:
- Can't connect to Redis (check `REDIS_URL`)
- Invalid configuration (check environment variables)
- Out of memory (increase limits)

### Workers Not Registering

```bash
# Check if workers can reach Redis
docker compose exec worker sh -c "nc -zv redis 6379"

# Check registered workers
docker compose exec redis redis-cli KEYS "worker:*"
```

### High Memory Usage

```bash
# Check container stats
docker stats

# Reduce concurrent sessions
environment:
  - MAX_CONCURRENT_SESSIONS=3
```

---

## Commands Reference

| Task | Command |
|------|---------|
| Start | `docker compose up -d` |
| Stop | `docker compose down` |
| View logs | `docker compose logs -f` |
| Scale workers | `docker compose up -d --scale worker=N` |
| Rebuild | `docker compose build --no-cache` |
| Check status | `docker compose ps` |
| View stats | `docker stats` |
| Enter container | `docker compose exec proxy sh` |

---

## Next Steps

- **[Kubernetes](kubernetes.md)** — For auto-scaling and high availability
- **[Scaling Strategies](scaling.md)** — Plan your capacity
- **[Monitoring](../operations/monitoring.md)** — Set up observability
