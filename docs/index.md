# playwright-distributed

**Self-hosted, horizontally-scalable Playwright grid.**

Connect your Playwright scripts to a single WebSocket endpoint and let the system handle the rest: worker selection, load balancing, and automatic browser recycling.

```python
# It's this simple
browser = await playwright.chromium.connect("ws://your-grid:8080")
```

---

## Why playwright-distributed?

| Challenge | How We Solve It |
|-----------|-----------------|
| Browser startup is slow | Workers keep browsers warm and ready |
| Memory leaks over time | Workers automatically restart after N sessions |
| Need more capacity | Add workers anytime; they self-register |
| Data privacy concerns | Self-hosted on your infrastructure |
| Multiple languages/teams | One endpoint works with any Playwright client |

---

## Quick Start

Get a working grid in under 5 minutes:

```bash
git clone https://github.com/mbroton/playwright-distributed.git
cd playwright-distributed
docker compose up -d
```

Connect from Python:

```python
from playwright.async_api import async_playwright

async with async_playwright() as p:
    browser = await p.chromium.connect("ws://localhost:8080")
    page = await browser.new_page()
    await page.goto("https://example.com")
    print(await page.title())
    await browser.close()
```

Or Node.js:

```javascript
import { chromium } from "playwright";

const browser = await chromium.connect("ws://localhost:8080");
const page = await browser.newPage();
await page.goto("https://example.com");
console.log(await page.title());
await browser.close();
```

**That's it.** The same endpoint works with Java, .NET, Go—any Playwright client.

---

## Documentation

### Just Getting Started?

**[Tutorial](tutorial/index.md)** — Learn by doing. Go from zero to a working grid, then scale it up.

1. [Your First Connection](tutorial/first-connection.md) — 5 minutes to a working grid
2. [Multiple Browsers](tutorial/multi-browser.md) — Add Firefox and WebKit
3. [Scaling Workers](tutorial/scaling-workers.md) — Handle more load
4. [Production Checklist](tutorial/production-checklist.md) — Before you go live

### Understanding the System

**[Concepts](concepts/index.md)** — How things work under the hood.

- [Architecture](concepts/architecture.md) — Components and how they interact
- [Connection Lifecycle](concepts/connection-lifecycle.md) — What happens when you connect
- [Worker Selection](concepts/worker-selection.md) — How the proxy chooses workers
- [Worker Lifecycle](concepts/worker-lifecycle.md) — Heartbeats, draining, restarts

### Configuration

**[Configuration Guide](configuration/index.md)** — All the knobs you can turn.

- [Proxy Configuration](configuration/proxy.md) — Proxy environment variables
- [Worker Configuration](configuration/worker.md) — Worker environment variables
- [Timing Configuration](configuration/timing.md) — Critical timing dependencies
- [Networking](configuration/networking.md) — Docker and Kubernetes networking

### Deployment

**[Deployment Guide](deployment/index.md)** — Run it in production.

- [Docker Compose](deployment/docker-compose.md) — Simple deployments
- [Kubernetes](deployment/kubernetes.md) — Container orchestration
- [Scaling Strategies](deployment/scaling.md) — When and how to scale

### Operations

**[Operations Guide](operations/index.md)** — Keep it running smoothly.

- [Monitoring](operations/monitoring.md) — What to watch
- [Troubleshooting](operations/troubleshooting.md) — When things go wrong
- [Maintenance](operations/maintenance.md) — Updates and restarts

### Code Examples

**[Examples](examples/index.md)** — Copy-paste ready code.

- [Python Examples](examples/python.md) — Basic usage to connection pools
- [Node.js Examples](examples/nodejs.md) — Express integration and more
- [Other Languages](examples/other-languages.md) — Go, .NET, Java

### Reference

**[Reference](reference/index.md)** — Technical details.

- [API Reference](reference/api.md) — WebSocket endpoints
- [Redis Schema](reference/redis-schema.md) — Internal data structures
- [Error Codes](reference/error-codes.md) — What errors mean

---

## Use Cases

| Scenario | Why It Works |
|----------|--------------|
| **AI/LLM Agents** | Give each agent an isolated browser with zero startup cost |
| **Web Scraping** | Scale throughput by adding workers; remove them to save money |
| **CI/CD Testing** | Parallelize test runs across many browsers |
| **Synthetic Monitoring** | Run browser checks from multiple locations |
| **Shared Browser Service** | One endpoint for your whole team |

---

## Architecture at a Glance

```
┌─────────────────────────────────────────────────────────────┐
│                    Your Application                          │
│         (Python, Node.js, Java, .NET, Go, etc.)             │
└─────────────────────────┬───────────────────────────────────┘
                          │ WebSocket
                          ▼
┌─────────────────────────────────────────────────────────────┐
│                         Proxy                                │
│              Single entry point for all clients              │
│                    ws://grid:8080                            │
└─────────────────────────┬───────────────────────────────────┘
                          │
              ┌───────────┼───────────┐
              │           │           │
              ▼           ▼           ▼
         ┌────────┐  ┌────────┐  ┌────────┐
         │ Worker │  │ Worker │  │ Worker │
         │ Chrome │  │ Firefox│  │ WebKit │
         └────────┘  └────────┘  └────────┘
              │           │           │
              └───────────┼───────────┘
                          │
                          ▼
                    ┌──────────┐
                    │  Redis   │
                    │ Registry │
                    └──────────┘
```

**How it works:**

1. Your code connects to the proxy via WebSocket
2. Proxy queries Redis for available workers
3. Proxy selects the best worker and connects to it
4. Messages flow bidirectionally through the proxy
5. When you disconnect, resources are cleaned up automatically

---

## Getting Help

- **[FAQ](faq.md)** — Common questions answered
- **[Troubleshooting](operations/troubleshooting.md)** — Debug common issues
- **[GitHub Issues](https://github.com/mbroton/playwright-distributed/issues)** — Report bugs or request features

---

## Next Steps

Ready to get started? Head to the **[Tutorial](tutorial/index.md)** and have a working grid in 5 minutes.
