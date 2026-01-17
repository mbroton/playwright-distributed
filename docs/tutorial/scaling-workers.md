# Scaling Workers

**Time: 15 minutes**

One worker can handle several concurrent connections, but eventually you'll need more. Let's learn how to scale the grid horizontally.

---

## What You'll Do

1. Understand worker capacity
2. Scale workers with Docker Compose
3. Observe load distribution
4. Understand the selection algorithm

---

## Understanding Worker Capacity

Each worker has two limits:

| Limit | Default | Purpose |
|-------|---------|---------|
| `MAX_CONCURRENT_SESSIONS` | 5 | Maximum simultaneous connections |
| `MAX_LIFETIME_SESSIONS` | 50 | Total connections before restart |

**Concurrent sessions**: How many browser contexts can run at once. This is limited by memory—each context uses RAM.

**Lifetime sessions**: After serving this many total connections, the worker restarts. This prevents memory leaks from accumulating.

---

## Step 1: Check Current Capacity

With the default setup (1 worker), you can handle:
- 5 concurrent connections
- 50 total connections before the worker restarts

Let's test this. Start the grid:

```bash
docker compose up -d
```

Create a script that opens multiple connections:

### Python

```python
import asyncio
from playwright.async_api import async_playwright

async def use_browser(playwright, session_id):
    """Simulate a browser session."""
    print(f"Session {session_id}: Connecting...")
    browser = await playwright.chromium.connect("ws://localhost:8080")

    try:
        page = await browser.new_page()
        await page.goto("https://example.com")
        title = await page.title()
        print(f"Session {session_id}: Got '{title}'")

        # Simulate some work
        await asyncio.sleep(2)

    finally:
        await browser.close()
        print(f"Session {session_id}: Done")

async def main():
    async with async_playwright() as p:
        # Try to run 3 concurrent sessions
        await asyncio.gather(
            use_browser(p, 1),
            use_browser(p, 2),
            use_browser(p, 3),
        )

asyncio.run(main())
```

Run it:

```bash
python test_concurrent.py
```

All three sessions should complete successfully. Now try with 10 concurrent sessions and see what happens as you approach the limit.

---

## Step 2: Scale with Docker Compose

To handle more load, add more workers. With Docker Compose, it's one command:

```bash
docker compose up -d --scale worker=3
```

Now you have:
- 3 workers × 5 concurrent = **15 concurrent sessions**
- 3 workers × 50 lifetime = **150 sessions before any restart**

Check they're all running:

```bash
docker compose ps
```

You should see three worker containers.

---

## Step 3: Observe Load Distribution

Let's see how the proxy distributes connections across workers.

Watch the proxy logs in one terminal:

```bash
docker compose logs -f proxy
```

In another terminal, run multiple connections:

```python
import asyncio
from playwright.async_api import async_playwright

async def quick_session(playwright, session_id):
    browser = await playwright.chromium.connect("ws://localhost:8080")
    page = await browser.new_page()
    await page.goto("https://example.com")
    await browser.close()
    print(f"Session {session_id} complete")

async def main():
    async with async_playwright() as p:
        # Run 10 quick sessions
        tasks = [quick_session(p, i) for i in range(10)]
        await asyncio.gather(*tasks)

asyncio.run(main())
```

In the proxy logs, you'll see which worker handles each connection. You might notice something interesting: connections aren't evenly distributed.

---

## Understanding the Selection Algorithm

The proxy doesn't use simple round-robin. Instead, it uses a **lifetime-first** strategy designed to stagger worker restarts.

### The Problem with Even Distribution

If all workers get equal load, they all hit their lifetime limit at the same time:

```
Time 0:   Worker A: 0    Worker B: 0    Worker C: 0
Time 10:  Worker A: 17   Worker B: 17   Worker C: 16
Time 20:  Worker A: 33   Worker B: 33   Worker C: 34
Time 30:  Worker A: 50   Worker B: 50   Worker C: 50  ← All restart!
          ↓              ↓              ↓
          [CAPACITY DROP: 0 workers available briefly]
```

### The Solution: Staggered Restarts

The proxy intentionally pushes one worker to its limit first:

```
Time 0:   Worker A: 0    Worker B: 0    Worker C: 0
Time 10:  Worker A: 30   Worker B: 12   Worker C: 8
Time 15:  Worker A: 50   Worker B: 20   Worker C: 15
          ↓ restarts
Time 16:  Worker A: 0    Worker B: 25   Worker C: 18
Time 25:  Worker A: 20   Worker B: 50   Worker C: 30
                         ↓ restarts
```

Only one worker restarts at a time, maintaining cluster capacity.

### How It Works

1. **Primary selection**: Pick the worker with the highest lifetime count (but still under a safety margin)
2. **Tie-breaker**: If lifetime counts are equal, pick the one with fewer active connections
3. **Fallback**: If all workers are near their limit, still route to one that can handle one more

For deep details, see [Worker Selection](../concepts/worker-selection.md).

---

## Scaling Guidelines

### How Many Workers?

Consider:

| Factor | Impact |
|--------|--------|
| Peak concurrent users | Need enough workers to handle max simultaneous connections |
| Session duration | Longer sessions = fewer concurrent slots available |
| Memory per worker | Each browser + contexts uses RAM |
| Restart frequency | More workers = less impact when one restarts |

**Rule of thumb**: Start with `peak_concurrent_users / 4` workers (assuming 5 concurrent sessions each), then adjust based on observed behavior.

### When to Scale

Add workers when:
- You see 503 errors ("no available servers")
- Response times increase (workers are at capacity)
- You're approaching `MAX_CONCURRENT_SESSIONS × worker_count`

Remove workers when:
- Most workers are idle
- Costs exceed value

### Scaling in Production

For production, use proper orchestration:

**Docker Compose** (simple):
```bash
docker compose up -d --scale worker=5
```

**Kubernetes** (recommended for production):
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: playwright-worker
spec:
  replicas: 5  # Adjust as needed
  ...
```

See [Kubernetes Deployment](../deployment/kubernetes.md) for full details.

---

## Monitoring Scale

Check the proxy's metrics endpoint:

```bash
curl http://localhost:8080/metrics
```

Returns:
```json
{"activeConnections": 7}
```

This shows total active connections across all workers.

For per-worker stats, query Redis:

```bash
docker compose exec redis redis-cli HGETALL cluster:active_connections
```

---

## Cleaning Up

Scale back down:

```bash
docker compose up -d --scale worker=1
```

Or stop everything:

```bash
docker compose down
```

---

## Next Steps

You know how to scale. Before going to production, let's run through the checklist:

**[Production Checklist](production-checklist.md)** — Make sure you're ready →

---

## Quick Reference

| Task | Command |
|------|---------|
| Scale to N workers | `docker compose up -d --scale worker=N` |
| Check worker count | `docker compose ps` |
| Check active connections | `curl http://localhost:8080/metrics` |
| Watch proxy logs | `docker compose logs -f proxy` |
