# Worker Lifecycle

Workers have a defined lifecycle: they start, register, serve connections, and eventually restart. Understanding this cycle helps you operate the grid reliably.

---

## Lifecycle Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Worker Lifecycle                             │
├─────────────────────────────────────────────────────────────────────┤
│                                                                      │
│   ┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐  │
│   │  START   │────►│  SERVE   │────►│  DRAIN   │────►│   EXIT   │  │
│   └──────────┘     └──────────┘     └──────────┘     └──────────┘  │
│        │                │                │                │         │
│        │                │                │                │         │
│        ▼                ▼                ▼                ▼         │
│   • Launch browser  • Accept        • Stop accepting  • Close      │
│   • Connect Redis     connections     new connections   browser    │
│   • Register        • Send          • Wait for        • Remove     │
│   • Start heartbeat   heartbeats      active to close   from Redis │
│                                     • Timeout if stuck • Exit      │
│                                                                      │
└─────────────────────────────────────────────────────────────────────┘
                                │
                                │ (Docker restarts)
                                │
                                ▼
                         Back to START
```

---

## Phase 1: Start

When a worker container starts:

### 1. Connect to Redis

```
Worker                    Redis
   │                        │
   │──PING─────────────────►│
   │◄──PONG─────────────────│
   │                        │
```

If Redis isn't available, the worker retries (up to `REDIS_RETRY_ATTEMPTS` times).

### 2. Launch Browser

```typescript
const server = await browserType.launchServer({
  headless: true,
  // ... other options
});
```

This starts a real browser process (Chromium, Firefox, or WebKit) and exposes it via WebSocket.

### 3. Register in Redis

```
Worker                    Redis
   │                        │
   │──HSET worker:chromium:abc123 ──────────────────►│
   │   browserType: chromium                          │
   │   wsEndpoint: ws://worker:3131/playwright/...   │
   │   status: available                              │
   │   lastHeartbeat: 1699999999                     │
   │                                                  │
   │──HSET cluster:active_connections ──────────────►│
   │   chromium:abc123: 0                            │
   │                                                  │
   │──HSET cluster:lifetime_connections ────────────►│
   │   chromium:abc123: 0                            │
   │                                                  │
   │──EXPIRE worker:chromium:abc123 TTL ───────────►│
```

The worker is now discoverable by the proxy.

### 4. Start Heartbeat Loop

A background loop sends heartbeats every `HEARTBEAT_INTERVAL` seconds.

---

## Phase 2: Serve

The worker is now active and serving connections.

### Heartbeat Loop

Every `HEARTBEAT_INTERVAL` seconds:

```
Worker                    Redis
   │                        │
   │  Every 10 seconds:     │
   │                        │
   │──HSET worker:id        │
   │   lastHeartbeat: now ─►│
   │                        │
   │──EXPIRE worker:id TTL─►│
   │                        │
   │──GET worker:cmd:id ───►│
   │◄──(nil or "shutdown")──│
   │                        │
```

The heartbeat:
- Updates the `lastHeartbeat` timestamp
- Refreshes the key's TTL (prevents expiration)
- Checks for shutdown commands

### Serving Connections

The browser server handles incoming WebSocket connections from the proxy:

```
Proxy                    Worker (Browser Server)
   │                        │
   │──WebSocket Connect ───►│
   │◄──Accepted─────────────│
   │                        │
   │══Playwright Protocol══►│
   │◄══════════════════════│
   │                        │
```

Each connection gets an isolated browser context.

### What the Worker Tracks

The worker itself tracks:
- Number of active connections (for logging)
- Whether it's received a shutdown command

The proxy tracks connection counts in Redis—the worker doesn't need to.

---

## Phase 3: Drain

When the worker receives a shutdown command, it enters drain mode.

### Trigger

The proxy sends a shutdown command when the worker's lifetime connections reach `MAX_LIFETIME_SESSIONS`:

```
Proxy                    Redis                   Worker
   │                        │                        │
   │──SET worker:cmd:id    ►│                        │
   │   "shutdown"           │                        │
   │   EX 60                │                        │
   │                        │                        │
```

### Worker Receives Command

On the next heartbeat:

```
Worker                    Redis
   │                        │
   │──GET worker:cmd:id ───►│
   │◄──"shutdown" ──────────│
   │                        │
```

### Enter Drain Mode

```
Worker                    Redis
   │                        │
   │──HSET worker:id       ►│
   │   status: draining     │
   │                        │
```

In drain mode:
- **No new connections accepted** (proxy won't select this worker)
- **Existing connections continue** (until they close naturally)

### Wait for Connections to Close

```javascript
while (activeConnections > 0 && !timeout) {
  await sleep(1000);
  // Check if connections have closed
}
```

The worker waits up to `DRAIN_TIMEOUT` (default 30 seconds) for active connections to close.

### Timeout Protection

If connections don't close within the timeout, the worker proceeds to exit anyway. This prevents a stuck connection from blocking restarts forever.

---

## Phase 4: Exit

### Cleanup

```
Worker                    Redis
   │                        │
   │──DEL worker:id ───────►│
   │──HDEL cluster:active ─►│
   │──HDEL cluster:life ───►│
   │                        │
```

### Close Browser

```typescript
await browserServer.close();
```

This terminates the browser process and all its contexts.

### Exit Process

```typescript
process.exit(0);
```

The worker process exits cleanly.

### Container Restart

With Docker's `restart: unless-stopped` policy:

```yaml
services:
  worker:
    restart: unless-stopped
```

Docker automatically starts a new worker container, and the cycle begins again.

---

## Abnormal Termination

Not all exits are graceful.

### Worker Crash

If the worker process crashes:
1. Active connections are immediately lost
2. Redis keys remain (until TTL expires)
3. Docker restarts the container
4. Proxy's reaper eventually cleans up stale keys

### Heartbeat Failure

If heartbeats stop (network issue, deadlock):
1. Redis key TTL expires
2. Worker becomes invisible to selection
3. Reaper cleans up stale records
4. (Worker may still be running but unreachable)

### Kill Signal

If the container receives SIGTERM/SIGKILL:
1. Worker may or may not have time to cleanup
2. Redis keys may be left behind
3. Reaper cleans up eventually

---

## The Reaper

The proxy runs a background process called the "reaper" that cleans up stale worker records.

### What It Does

Every `REAPER_RUN_INTERVAL` seconds (default 5 minutes):

```lua
-- Find workers with stale heartbeats
for each worker in Redis:
  if (now - lastHeartbeat) > STALE_THRESHOLD:
    delete worker keys
    log "Reaped stale worker"
```

### Why It's Needed

- Workers that crash don't clean up their keys
- Network partitions can leave orphan records
- Provides a safety net for any unexpected state

### Configuration

| Setting | Default | Purpose |
|---------|---------|---------|
| `REAPER_RUN_INTERVAL` | 300s | How often to check for stale workers |
| Stale threshold | 30s | How old a heartbeat can be before reaping |

---

## Timing Relationships

These timings must be configured correctly:

```
Heartbeat Interval (10s)
        ↓
        must be less than
        ↓
Key TTL (30s)           Stale Threshold (30s)
        ↓                        ↓
        must be less than        must be less than
        ↓                        ↓
Shutdown Command TTL (60s)    Reaper Interval (300s)
```

**If TTL < Heartbeat**: Worker appears dead while healthy
**If Stale < Heartbeat**: Reaper kills healthy workers
**If Command TTL < Heartbeat**: Shutdown commands expire before seen

See [Timing Configuration](../configuration/timing.md) for details.

---

## Monitoring Lifecycle

### Check Worker Status

```bash
# All workers
redis-cli KEYS "worker:*:*"

# Specific worker details
redis-cli HGETALL worker:chromium:abc123
```

### Watch for Restarts

```bash
# In proxy logs
docker compose logs -f proxy | grep -i "shutdown\|restart"

# In worker logs
docker compose logs -f worker | grep -i "drain\|shutdown"
```

### Identify Stale Workers

```bash
# Workers with old heartbeats
redis-cli HGETALL worker:chromium:abc123
# Check lastHeartbeat against current time
```

---

## Best Practices

### Set Appropriate Lifetime Limits

- Too low: Frequent restarts, more disruption
- Too high: Memory accumulation, degraded performance

Start with 50-100 and adjust based on memory patterns.

### Ensure Proper Restart Policy

```yaml
services:
  worker:
    restart: unless-stopped  # or "always"
```

Without this, workers won't come back after exiting.

### Monitor Drain Duration

If drains consistently timeout:
- Connections may be leaking
- Clients may not be closing properly
- Consider shorter drain timeout

### Keep Workers Stateless

Don't store state in workers—they restart frequently. Use external storage for any persistent data.

---

## Next

- **[Timing Configuration](../configuration/timing.md)** — Configure heartbeats and timeouts
- **[Troubleshooting](../operations/troubleshooting.md)** — Debug lifecycle issues
