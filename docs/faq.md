# Frequently Asked Questions

Common questions about playwright-distributed.

---

## General

### What is playwright-distributed?

playwright-distributed is a self-hosted, horizontally-scalable Playwright browser grid. It lets you connect your Playwright scripts to a single WebSocket endpoint that routes requests to a pool of browser workers.

### How is it different from running Playwright locally?

| Local Playwright | playwright-distributed |
|------------------|------------------------|
| Browser starts with each script | Browsers are always warm |
| Uses local machine resources | Uses pool of workers |
| One browser at a time (typically) | Many concurrent browsers |
| Memory leaks accumulate | Workers restart automatically |

### What languages are supported?

Any language with a Playwright client:
- Python
- Node.js/JavaScript/TypeScript
- Java
- .NET (C#)
- Go (community library)
- Ruby (community library)

### Is it free?

Yes, playwright-distributed is open source under the Apache 2.0 license. You pay only for the infrastructure you run it on.

---

## Setup & Deployment

### How do I get started?

```bash
git clone https://github.com/mbroton/playwright-distributed.git
cd playwright-distributed
docker compose up -d
```

Then connect:
```python
browser = await playwright.chromium.connect("ws://localhost:8080")
```

### What are the minimum requirements?

- **Docker** and **Docker Compose**
- **1 GB RAM** minimum per worker
- **Redis** (included in Docker Compose)

### Can I run it without Docker?

Yes, but it's more complex:
1. Build the Go proxy from source
2. Run Node.js workers with Playwright installed
3. Set up Redis
4. Configure networking manually

Docker is strongly recommended.

### How do I run multiple browser types?

Use the `docker-compose.local.yaml` file or create separate worker services:

```yaml
worker-chromium:
  environment:
    - BROWSER_TYPE=chromium
    - PRIVATE_HOSTNAME=worker-chromium

worker-firefox:
  environment:
    - BROWSER_TYPE=firefox
    - PRIVATE_HOSTNAME=worker-firefox
```

---

## Usage

### How do I connect to a specific browser?

Use the `browser` query parameter:

```python
# Chromium (default)
await p.chromium.connect("ws://localhost:8080")

# Firefox
await p.firefox.connect("ws://localhost:8080?browser=firefox")

# WebKit
await p.webkit.connect("ws://localhost:8080?browser=webkit")
```

### Can I reuse connections?

Yes, and you should! Each connection has overhead. Create contexts on an existing connection:

```python
browser = await p.chromium.connect("ws://localhost:8080")

# Reuse browser, create new contexts
for url in urls:
    context = await browser.new_context()
    page = await context.new_page()
    await page.goto(url)
    await context.close()

await browser.close()
```

### How many concurrent connections can I have?

`Workers × MAX_CONCURRENT_SESSIONS`

Default: 1 worker × 5 sessions = 5 concurrent connections

Scale by adding workers or increasing `MAX_CONCURRENT_SESSIONS`.

### Why do workers restart?

Workers restart after serving `MAX_LIFETIME_SESSIONS` connections. This prevents:
- Memory leaks from accumulating
- Browser slowdowns over time
- Unpredictable behavior

It's intentional and healthy.

---

## Troubleshooting

### I get "no available servers" error

This means no workers can accept your connection. Common causes:

1. **No workers running**: `docker compose ps`
2. **All workers busy**: Check active connections with `curl http://localhost:8080/metrics`
3. **Wrong browser type**: Make sure you have workers for the browser you're requesting
4. **Workers not registered**: Check `docker compose exec redis redis-cli KEYS "worker:*"`

See [Troubleshooting](operations/troubleshooting.md) for detailed steps.

### Connections are slow

1. Check worker CPU/memory: `docker stats`
2. Check if at capacity: Add more workers
3. Check Redis latency: Should be < 10ms

### Workers keep crashing

Check logs:
```bash
docker compose logs worker
```

Common causes:
- Out of memory (increase limits)
- Problematic URLs (browser crashes)
- Configuration errors

### How do I enable debug logging?

```yaml
environment:
  - LOG_LEVEL=debug
```

Then: `docker compose up -d && docker compose logs -f`

---

## Architecture

### Why use a proxy instead of direct connections?

1. **Security**: Workers don't need public exposure
2. **Simplicity**: One endpoint for all workers
3. **Load balancing**: Proxy handles worker selection
4. **Tracking**: Central connection counting

### Why Redis?

Redis provides:
- Shared state between proxy and workers
- Fast key-value lookups
- Atomic operations (Lua scripts)
- Key expiration for automatic cleanup

### Can I run multiple proxies?

Yes! Proxies are stateless. Run multiple behind a load balancer for high availability.

### What happens if Redis goes down?

- New connections fail (can't select workers)
- Existing connections continue working
- Workers keep running but can't register

Redis is critical. Use persistence and/or replication for production.

---

## Scaling

### How do I scale workers?

```bash
docker compose up -d --scale worker=10
```

Or in Kubernetes:
```bash
kubectl scale deployment playwright-worker --replicas=10
```

### When should I add more workers?

- Active connections near capacity
- 503 errors increasing
- Response times increasing

### Can I auto-scale?

Yes, with Kubernetes HPA or cloud auto-scaling. Scale based on CPU, memory, or custom metrics (active connections).

### How many workers do I need?

```
Workers = Peak concurrent connections / MAX_CONCURRENT_SESSIONS
```

Add 20-30% buffer for restarts and spikes.

---

## Security

### Is it secure to expose?

The proxy is designed to be the only exposed component. But:
- Consider adding authentication (reverse proxy)
- Use TLS in production
- Limit network access to Redis and workers

### Does it support authentication?

Not natively. Add authentication via:
- Reverse proxy (nginx, Traefik)
- Cloud load balancer (ALB, etc.)

### Can browsers access my internal network?

Yes, browsers can make requests to any reachable address. If this is a concern:
- Use network policies to restrict worker egress
- Run workers in isolated networks

---

## Comparison

### vs. Browserless

| | playwright-distributed | Browserless |
|-|------------------------|-------------|
| Hosting | Self-hosted | SaaS or self-hosted |
| Cost | Infrastructure only | Subscription |
| Features | Core grid | Additional tools |
| Complexity | Simple | More features |

### vs. Selenium Grid

| | playwright-distributed | Selenium Grid |
|-|------------------------|---------------|
| Protocol | Playwright (WebSocket) | WebDriver |
| Browsers | Chrome, Firefox, WebKit | Many |
| Performance | Fast (WebSocket) | Slower (HTTP) |
| API | Playwright | Selenium |

### vs. Local Playwright

| | playwright-distributed | Local |
|-|------------------------|-------|
| Browser startup | Instant (warm) | 2-10 seconds |
| Concurrent browsers | Many | Limited by machine |
| Memory | Distributed | Single machine |
| Setup | Grid deployment | pip/npm install |

---

## Contributing

### How can I contribute?

- Report bugs via [GitHub Issues](https://github.com/mbroton/playwright-distributed/issues)
- Submit pull requests
- Improve documentation
- Share your use cases

### Where do I report bugs?

[GitHub Issues](https://github.com/mbroton/playwright-distributed/issues)

Include:
- Clear description
- Steps to reproduce
- Logs and configuration
- Expected vs actual behavior

---

## Still Have Questions?

- Check [Troubleshooting](operations/troubleshooting.md) for common issues
- Search [GitHub Issues](https://github.com/mbroton/playwright-distributed/issues)
- Open a new issue if needed
