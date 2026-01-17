# Architecture

playwright-distributed has three components: the proxy, workers, and Redis. Understanding how they interact helps you deploy and troubleshoot effectively.

---

## The Big Picture

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Your Applications                             │
│              (Python, Node.js, Java, .NET, Go, etc.)                │
└────────────────────────────────┬────────────────────────────────────┘
                                 │
                                 │ WebSocket (public)
                                 │ ws://grid:8080
                                 ▼
┌─────────────────────────────────────────────────────────────────────┐
│                              Proxy                                   │
│                                                                      │
│  • Single entry point for all clients                               │
│  • Routes connections to appropriate workers                         │
│  • Relays WebSocket messages bidirectionally                        │
│  • Tracks active connections                                         │
│  • Triggers worker restarts when needed                             │
└────────────────────────────────┬────────────────────────────────────┘
                                 │
                 ┌───────────────┼───────────────┐
                 │               │               │
                 ▼               ▼               ▼
          ┌──────────┐   ┌──────────┐   ┌──────────┐
          │  Worker  │   │  Worker  │   │  Worker  │
          │ Chromium │   │ Firefox  │   │  WebKit  │
          └────┬─────┘   └────┬─────┘   └────┬─────┘
               │              │              │
               │   WebSocket (private)       │
               │              │              │
               └──────────────┼──────────────┘
                              │
                              ▼
                       ┌──────────────┐
                       │    Redis     │
                       │              │
                       │ • Registry   │
                       │ • Heartbeats │
                       │ • Counters   │
                       │ • Commands   │
                       └──────────────┘
```

---

## Components

### Proxy

**Language**: Go

**Purpose**: The single entry point for all client connections.

**Responsibilities**:
- Accept WebSocket connections from clients
- Select appropriate workers based on browser type and load
- Establish connections to selected workers
- Relay messages bidirectionally between clients and workers
- Track active and lifetime connection counts
- Send shutdown commands when workers hit their session limit
- Run a reaper to clean up stale worker records

**Key characteristics**:
- Stateless (all state is in Redis)
- Can run multiple instances behind a load balancer
- Never stores browser data

### Worker

**Language**: TypeScript (Node.js)

**Purpose**: Run browser instances and serve automation requests.

**Responsibilities**:
- Launch and maintain a Playwright browser server
- Register with Redis on startup
- Send periodic heartbeats to prove liveness
- Serve browser contexts to connected clients
- Drain gracefully when shutdown is requested
- Exit when told to restart

**Key characteristics**:
- One browser type per worker (Chromium, Firefox, or WebKit)
- Multiple concurrent contexts per browser
- Automatically restarts after serving N sessions

### Redis

**Purpose**: Coordination layer for the entire system.

**Stores**:
- Worker registry (endpoints, status, browser type)
- Active connection counts per worker
- Lifetime connection counts per worker
- Heartbeat timestamps
- Shutdown commands

**Key characteristics**:
- Single point of truth for cluster state
- Uses Lua scripts for atomic operations
- Keys have TTLs for automatic cleanup

---

## Communication Patterns

### Client → Proxy → Worker

When you call `browser.connect("ws://grid:8080")`:

1. **HTTP Upgrade**: Client sends WebSocket upgrade request to proxy
2. **Worker Selection**: Proxy queries Redis, selects a worker
3. **Backend Connection**: Proxy connects to worker's WebSocket endpoint
4. **Client Upgrade**: Proxy upgrades client connection to WebSocket
5. **Message Relay**: Proxy forwards messages in both directions

```
Client                   Proxy                    Worker
  │                        │                        │
  │──HTTP Upgrade ────────►│                        │
  │                        │──Select Worker ───────►│ Redis
  │                        │◄──Worker Endpoint ─────│
  │                        │                        │
  │                        │──WebSocket Connect ───►│
  │                        │◄──Connected ───────────│
  │                        │                        │
  │◄─WebSocket Upgrade ────│                        │
  │                        │                        │
  │══Playwright Messages══►│══════════════════════►│
  │◄══════════════════════│◄══════════════════════│
  │                        │                        │
```

### Worker → Redis (Heartbeats)

Workers send heartbeats every N seconds:

```
Worker                   Redis
  │                        │
  │──HSET worker:id {...}─►│  Update metadata
  │──EXPIRE worker:id TTL─►│  Refresh TTL
  │──GET worker:cmd:id ───►│  Check for commands
  │◄──shutdown ────────────│  (if shutdown requested)
  │                        │
```

### Proxy → Worker (Shutdown)

When a worker hits its lifetime limit:

```
Proxy                    Redis                   Worker
  │                        │                        │
  │──SET worker:cmd:id ───►│                        │
  │   "shutdown"           │                        │
  │                        │                        │
  │                        │  (next heartbeat)      │
  │                        │◄──GET worker:cmd:id ───│
  │                        │──"shutdown" ──────────►│
  │                        │                        │
  │                        │                        │──Enter drain mode
  │                        │                        │──Wait for connections
  │                        │                        │──Exit
```

---

## Design Decisions

### Why Proxy-Based (Not Direct Connection)?

We could let clients connect directly to workers. Instead, we route through a proxy. Why?

| Direct Connection | Proxy-Based |
|-------------------|-------------|
| Expose worker IPs to clients | Workers stay private |
| Complex client-side selection | Simple single endpoint |
| Hard to track connections | Central connection tracking |
| Client handles failover | Proxy handles failover |

The proxy adds a hop but dramatically simplifies operations and improves security.

### Why Redis (Not In-Memory)?

We could track workers in the proxy's memory. Why Redis?

| In-Memory | Redis |
|-----------|-------|
| State lost on restart | State persists |
| Single proxy only | Multiple proxies can share state |
| Complex coordination | Simple pub/sub and atomic ops |
| DIY consistency | Battle-tested data structures |

Redis enables horizontal scaling of proxies and survives proxy restarts.

### Why Restart Workers (Not Run Forever)?

Workers restart after N sessions. Why not run forever?

| Run Forever | Periodic Restart |
|-------------|------------------|
| Memory leaks accumulate | Fresh memory state |
| Browser gets slow | Consistent performance |
| Hard to update | Easy rolling updates |
| Unpredictable behavior | Predictable lifecycle |

Browsers accumulate state and slow down. Regular restarts keep performance consistent.

---

## Scaling

### Scaling Workers

Add more workers to increase capacity:

```
Capacity = Workers × MAX_CONCURRENT_SESSIONS
```

Workers are stateless—just add more containers.

### Scaling Proxies

For high availability, run multiple proxy instances behind a load balancer:

```
                    Load Balancer
                         │
            ┌────────────┼────────────┐
            ▼            ▼            ▼
         Proxy 1     Proxy 2      Proxy 3
            │            │            │
            └────────────┼────────────┘
                         │
                       Redis
```

All proxies share state through Redis.

### Scaling Redis

For most deployments, a single Redis instance is sufficient. For high availability:
- Redis Sentinel for automatic failover
- Redis Cluster for horizontal scaling (if you have thousands of workers)

---

## Network Requirements

| From | To | Protocol | Port |
|------|------|----------|------|
| Client | Proxy | WebSocket | 8080 (configurable) |
| Proxy | Worker | WebSocket | 3131 (configurable) |
| Proxy | Redis | TCP | 6379 |
| Worker | Redis | TCP | 6379 |

**Important**: Workers and Redis should NOT be publicly accessible. Only expose the proxy.

---

## Next

- **[Connection Lifecycle](connection-lifecycle.md)** — Detailed flow of a connection
- **[Worker Selection](worker-selection.md)** — How workers are chosen
