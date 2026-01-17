# Concepts

Understand how playwright-distributed works under the hood. This knowledge helps you configure, troubleshoot, and optimize your grid.

---

## Core Concepts

### [Architecture](architecture.md)

The three components and how they interact:
- **Proxy**: Routes connections to workers
- **Workers**: Run browser instances
- **Redis**: Coordinates everything

### [Connection Lifecycle](connection-lifecycle.md)

What happens from connect to disconnect:
- WebSocket upgrade
- Worker selection
- Message relaying
- Cleanup

### [Worker Selection](worker-selection.md)

How the proxy chooses which worker handles your connection:
- The lifetime-first algorithm
- Staggered restarts
- Safety margins

### [Worker Lifecycle](worker-lifecycle.md)

A worker's journey from startup to restart:
- Registration
- Heartbeats
- Draining
- Graceful shutdown

---

## Key Ideas

### Hub-as-Proxy Model

The proxy isn't just a load balancer—it's a full proxy. Your connection terminates at the proxy, and the proxy maintains a separate connection to the worker.

```
Client ←──WebSocket──→ Proxy ←──WebSocket──→ Worker
```

**Why this matters:**
- Workers never exposed publicly
- Proxy can track all connections centrally
- Clean disconnection handling

### Isolation Through Contexts

When you connect, you get a browser context, not a browser process. Multiple clients share the same browser process but have completely isolated contexts (cookies, storage, cache).

### Self-Healing Design

The system handles failures automatically:
- Dead workers are reaped from Redis
- Workers restart after N sessions (preventing memory leaks)
- New workers self-register when they start

---

## Reading Order

If you're new to the system:

1. Start with **[Architecture](architecture.md)** for the big picture
2. Read **[Connection Lifecycle](connection-lifecycle.md)** to understand the flow
3. Dive into **[Worker Selection](worker-selection.md)** if you're curious about load balancing
4. Check **[Worker Lifecycle](worker-lifecycle.md)** for operational understanding
