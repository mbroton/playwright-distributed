# Connection Lifecycle

Every connection goes through a defined lifecycle: establishment, active use, and termination. Understanding this helps you debug issues and optimize your code.

---

## Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                        Connection Lifecycle                      │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  1. ESTABLISHMENT                                               │
│     Client connects to proxy                                     │
│     Proxy selects worker                                         │
│     Proxy connects to worker                                     │
│     Connection established                                       │
│                                                                  │
│  2. ACTIVE                                                       │
│     Messages relay bidirectionally                               │
│     Browser context serves requests                              │
│                                                                  │
│  3. TERMINATION                                                  │
│     Client disconnects (or error)                                │
│     Proxy disconnects from worker                                │
│     Counters decremented                                         │
│     Resources cleaned up                                         │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

---

## Phase 1: Establishment

When you call `browser.connect("ws://grid:8080")`:

### Step 1: HTTP Request

Your Playwright client sends an HTTP request with WebSocket upgrade headers:

```http
GET /?browser=chromium HTTP/1.1
Host: grid:8080
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==
Sec-WebSocket-Version: 13
```

### Step 2: Worker Selection

The proxy:
1. Extracts the `browser` query parameter (defaults to "chromium")
2. Runs the selector Lua script in Redis
3. Receives a worker endpoint or waits (up to `WORKER_SELECT_TIMEOUT`)

```lua
-- Simplified selector logic
1. Find all workers with matching browserType
2. Filter: status="available", heartbeat recent, under limits
3. Select worker with highest lifetime (staggered restart strategy)
4. Increment active_connections and lifetime_connections
5. Return worker's WebSocket endpoint
```

If no worker is available within the timeout, the proxy returns:
```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/json

{"error": "no available servers"}
```

### Step 3: Backend Connection

The proxy connects to the selected worker's internal WebSocket endpoint:

```
Proxy ──WebSocket──► ws://worker:3131/playwright/chromium/...
```

This connection happens over the internal network—workers are never exposed publicly.

### Step 4: Client Upgrade

Once the backend connection is established, the proxy completes the WebSocket upgrade with the client:

```http
HTTP/1.1 101 Switching Protocols
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=
```

Now both connections are established, and message relaying begins.

---

## Phase 2: Active

During the active phase, the proxy relays messages bidirectionally:

```
Client                    Proxy                    Worker
   │                        │                        │
   │──Playwright Command───►│───────────────────────►│
   │                        │                        │──Execute
   │◄──────────────────────│◄──────Result───────────│
   │                        │                        │
   │──Navigate─────────────►│───────────────────────►│
   │                        │                        │──Load page
   │◄──────────────────────│◄──────Done─────────────│
   │                        │                        │
```

### What the Proxy Does

- **Relay**: Forward messages unchanged in both directions
- **Track**: Maintain the connection count for the worker
- **Monitor**: Detect disconnections on either side

### What the Proxy Doesn't Do

- **Inspect**: It doesn't parse or modify Playwright protocol messages
- **Buffer**: Messages are streamed, not buffered
- **Transform**: No protocol translation

### Connection State

During the active phase, the connection is tracked in Redis:

```
worker:chromium:abc123
├── status: "available"
├── activeConnections: 3  (this connection is one of them)
├── lifetimeConnections: 47
└── lastHeartbeat: 1699999999

cluster:active_connections
└── chromium:abc123: 3

cluster:lifetime_connections
└── chromium:abc123: 47
```

---

## Phase 3: Termination

Connections end for several reasons:

### Normal Termination (Client Closes)

Most common case—your code calls `browser.close()`:

```
Client                    Proxy                    Worker
   │                        │                        │
   │──Close Frame──────────►│                        │
   │                        │──Close Frame──────────►│
   │                        │◄─Close Ack────────────│
   │◄──Close Ack───────────│                        │
   │                        │                        │
   │                        │──Decrement counters───►│ Redis
```

### Error Termination (Connection Drops)

Network issues, crashes, or timeouts:

```
Client                    Proxy                    Worker
   │                        │                        │
   │      ✕ Connection lost │                        │
   │                        │                        │
   │                        │──Close Frame──────────►│
   │                        │◄─Close Ack────────────│
   │                        │                        │
   │                        │──Decrement counters───►│ Redis
```

### Backend Error (Worker Dies)

If the worker crashes or disconnects:

```
Client                    Proxy                    Worker
   │                        │                        │
   │                        │      ✕ Connection lost │
   │                        │                        │
   │◄──Close Frame─────────│                        │
   │──Close Ack───────────►│                        │
   │                        │                        │
   │                        │──Decrement counters───►│ Redis
```

Your Playwright client will see a connection error.

---

## Counter Management

The proxy maintains two counters per worker:

### Active Connections

- **Incremented**: When a connection is established
- **Decremented**: When a connection terminates
- **Limit**: `MAX_CONCURRENT_SESSIONS`
- **Purpose**: Prevent overloading a worker

### Lifetime Connections

- **Incremented**: When a connection is established
- **Never decremented**: Monotonically increasing
- **Limit**: `MAX_LIFETIME_SESSIONS`
- **Purpose**: Trigger worker restart after N total sessions

When lifetime connections reach the limit:

```
Proxy                    Redis                   Worker
   │                        │                        │
   │──SET worker:cmd:id────►│                        │
   │   "shutdown"           │                        │
   │                        │                        │
   │                        │ (next heartbeat)       │
   │                        │◄─GET worker:cmd:id ───│
   │                        │──"shutdown"──────────►│
```

---

## Timing

### Typical Connection Establishment

| Step | Duration |
|------|----------|
| HTTP request to proxy | < 10ms |
| Worker selection | < 5ms |
| Backend connection | < 50ms |
| **Total** | **< 100ms** |

Compare this to launching a fresh browser (2-10 seconds).

### Timeouts

| Timeout | Default | Configurable |
|---------|---------|--------------|
| Worker selection | 5s | `WORKER_SELECT_TIMEOUT` |
| HTTP write | 15s | Hardcoded |
| WebSocket ping/pong | 60s | Hardcoded |

---

## Debugging Connections

### Check Active Connections

```bash
# Total active connections
curl http://localhost:8080/metrics

# Per-worker breakdown
redis-cli HGETALL cluster:active_connections
```

### Watch Connection Flow

Enable debug logging:

```yaml
environment:
  - LOG_LEVEL=debug
```

Then watch the logs:

```bash
docker compose logs -f proxy
```

You'll see:
```
level=debug msg="Received WebSocket upgrade request" browser=chromium
level=debug msg="Selected worker" worker=chromium:abc123 endpoint=ws://worker:3131/...
level=debug msg="Backend connection established"
level=debug msg="Client connection upgraded"
level=debug msg="Starting message relay"
```

### Common Issues

| Symptom | Likely Cause |
|---------|--------------|
| Connection hangs at establishment | Worker selection timeout, network issues |
| Immediate 503 | No workers registered or all at capacity |
| Frequent disconnections | Worker crashes, network instability |
| Slow establishment | Redis latency, DNS resolution |

---

## Best Practices

### Reuse Connections

Creating a connection has overhead. Reuse when possible:

```python
# BAD: New connection per operation
for url in urls:
    browser = await p.chromium.connect("ws://grid:8080")
    page = await browser.new_page()
    await page.goto(url)
    await browser.close()

# GOOD: Reuse connection, create new contexts
browser = await p.chromium.connect("ws://grid:8080")
for url in urls:
    context = await browser.new_context()
    page = await context.new_page()
    await page.goto(url)
    await context.close()
await browser.close()
```

### Handle Disconnections

Connections can drop. Handle it gracefully:

```python
try:
    browser = await p.chromium.connect("ws://grid:8080")
    # ... use browser ...
except playwright.Error as e:
    if "disconnected" in str(e).lower():
        # Reconnect and retry
        pass
    raise
```

### Don't Hold Connections Idle

If you're not using a connection, close it. Idle connections consume worker capacity.

---

## Next

- **[Worker Selection](worker-selection.md)** — How workers are chosen
- **[Worker Lifecycle](worker-lifecycle.md)** — Heartbeats, draining, restarts
