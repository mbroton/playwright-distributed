# Timing Configuration

Timing values in playwright-distributed have dependencies. Misconfiguring them causes silent failures—workers appear dead, commands expire unseen, or connections hang. This guide explains the relationships.

---

## The Golden Rule

```
Heartbeat Interval < Key TTL < Shutdown Command TTL
```

Every timing value depends on the heartbeat interval. If you change the heartbeat, you must adjust everything else.

---

## Dependency Diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Timing Dependencies                           │
├─────────────────────────────────────────────────────────────────────┤
│                                                                      │
│   HEARTBEAT_INTERVAL (Worker)                                        │
│   └── The fundamental pulse of the system                           │
│       │                                                              │
│       ├──► REDIS_KEY_TTL (Worker)                                   │
│       │    Must be > HEARTBEAT_INTERVAL                             │
│       │    Recommended: 2-3×                                        │
│       │                                                              │
│       ├──► SHUTDOWN_COMMAND_TTL (Proxy)                             │
│       │    Must be > HEARTBEAT_INTERVAL                             │
│       │    Recommended: 4-6×                                        │
│       │                                                              │
│       └──► Reaper Stale Threshold (Hardcoded: 30s)                  │
│            Must be > HEARTBEAT_INTERVAL                             │
│                                                                      │
│   WORKER_SELECT_TIMEOUT (Proxy)                                      │
│   └── Must be < HTTP Write Timeout (Hardcoded: 15s)                 │
│                                                                      │
└─────────────────────────────────────────────────────────────────────┘
```

---

## Each Value Explained

### Heartbeat Interval

**Location**: Worker (`HEARTBEAT_INTERVAL`)
**Default**: 5 seconds
**Purpose**: How often workers send "I'm alive" signals

The heartbeat is the foundation. Every N seconds, the worker:
1. Updates `lastHeartbeat` timestamp in Redis
2. Refreshes the key TTL
3. Checks for shutdown commands

```
Worker ──[heartbeat]──► Redis
         every 5s
```

### Redis Key TTL

**Location**: Worker (`REDIS_KEY_TTL`)
**Default**: 60 seconds
**Purpose**: How long before a worker's Redis key expires

Each heartbeat refreshes this TTL. If heartbeats stop (worker dies), the key eventually expires and the worker becomes invisible.

**Rule**: `REDIS_KEY_TTL > HEARTBEAT_INTERVAL`

**Why**: If TTL is shorter than heartbeat, the key expires between heartbeats:

```
❌ Bad: HEARTBEAT_INTERVAL=10s, REDIS_KEY_TTL=5s

Time 0:  Heartbeat sent, TTL=5s
Time 5:  Key expires! (next heartbeat at Time 10)
         Worker appears dead even though it's healthy
```

**Recommendation**: `REDIS_KEY_TTL = 2× to 3× HEARTBEAT_INTERVAL`

### Shutdown Command TTL

**Location**: Proxy (`SHUTDOWN_COMMAND_TTL`)
**Default**: 60 seconds
**Purpose**: How long shutdown commands wait to be picked up

When a worker hits its lifetime limit, the proxy writes a shutdown command to Redis. The worker checks for this command during its next heartbeat.

**Rule**: `SHUTDOWN_COMMAND_TTL > HEARTBEAT_INTERVAL`

**Why**: The command must survive until the worker's next heartbeat:

```
❌ Bad: HEARTBEAT_INTERVAL=10s, SHUTDOWN_COMMAND_TTL=5s

Time 0:  Proxy sends shutdown command (TTL=5s)
Time 5:  Command expires!
Time 10: Worker heartbeat checks for command, finds nothing
         Worker never shuts down
```

**Recommendation**: `SHUTDOWN_COMMAND_TTL = 4× to 6× HEARTBEAT_INTERVAL`

This provides buffer for:
- Network delays
- Slightly delayed heartbeats
- Clock drift between containers

### Reaper Stale Threshold

**Location**: Proxy (hardcoded in `reaper.lua`)
**Default**: 30 seconds
**Purpose**: How old a heartbeat can be before the worker is considered dead

The reaper periodically scans for workers with stale heartbeats and removes them.

**Rule**: `Stale Threshold > HEARTBEAT_INTERVAL`

**Why**: You need to allow for missed heartbeats without false positives:

```
❌ Bad: HEARTBEAT_INTERVAL=10s, Stale Threshold=5s

Worker is healthy but heartbeat delayed by 6s
Reaper marks worker as stale and removes it
```

**Note**: This value is currently hardcoded. It should be at least 3× the heartbeat interval.

### Worker Select Timeout

**Location**: Proxy (`WORKER_SELECT_TIMEOUT`)
**Default**: 5 seconds
**Purpose**: How long to wait for an available worker

When no workers are available, the proxy waits this long before returning 503.

**Rule**: `WORKER_SELECT_TIMEOUT < HTTP Write Timeout (15s)`

**Why**: The proxy must respond before the HTTP connection times out:

```
❌ Bad: WORKER_SELECT_TIMEOUT=20s, HTTP timeout=15s

Time 0:  Client connects
Time 15: HTTP connection times out
Time 20: Proxy would have responded, but connection is gone
```

**Recommendation**: `WORKER_SELECT_TIMEOUT <= 10s`

---

## Safe Configurations

### Development (Fast Feedback)

```yaml
# Worker
HEARTBEAT_INTERVAL=5    # Quick heartbeats
REDIS_KEY_TTL=15        # 3× heartbeat

# Proxy
SHUTDOWN_COMMAND_TTL=30 # 6× heartbeat
WORKER_SELECT_TIMEOUT=3 # Fail fast
```

### Production (Stability)

```yaml
# Worker
HEARTBEAT_INTERVAL=10   # Standard heartbeats
REDIS_KEY_TTL=30        # 3× heartbeat

# Proxy
SHUTDOWN_COMMAND_TTL=60 # 6× heartbeat
WORKER_SELECT_TIMEOUT=5 # Reasonable wait
```

### High-Latency Network

```yaml
# Worker
HEARTBEAT_INTERVAL=15   # Longer between heartbeats
REDIS_KEY_TTL=60        # 4× heartbeat (extra buffer)

# Proxy
SHUTDOWN_COMMAND_TTL=90 # 6× heartbeat
WORKER_SELECT_TIMEOUT=10 # More time for worker discovery
```

---

## Validation Checklist

Before deploying, verify:

```
□ REDIS_KEY_TTL > HEARTBEAT_INTERVAL (at least 2×)
□ SHUTDOWN_COMMAND_TTL > HEARTBEAT_INTERVAL (at least 4×)
□ WORKER_SELECT_TIMEOUT < 15s (HTTP timeout)
□ Reaper threshold (30s) > HEARTBEAT_INTERVAL
```

---

## Debugging Timing Issues

### Symptom: Workers Appear Dead

```
"No available servers" even though workers are running
```

**Check**: Is `REDIS_KEY_TTL` greater than `HEARTBEAT_INTERVAL`?

```bash
# Check worker config
docker compose exec worker env | grep -E 'HEARTBEAT|TTL'

# Check if workers are registered
docker compose exec redis redis-cli KEYS "worker:*"

# Check heartbeat age
docker compose exec redis redis-cli HGET worker:chromium:abc123 lastHeartbeat
# Compare to current time
date +%s
```

### Symptom: Workers Never Restart

```
Worker lifetime exceeds MAX_LIFETIME_SESSIONS but doesn't restart
```

**Check**: Is `SHUTDOWN_COMMAND_TTL` greater than `HEARTBEAT_INTERVAL`?

```bash
# Check proxy config
docker compose exec proxy env | grep SHUTDOWN_COMMAND_TTL

# Look for shutdown commands
docker compose exec redis redis-cli KEYS "worker:cmd:*"

# Check worker logs for command receipt
docker compose logs worker | grep -i shutdown
```

### Symptom: Connections Hang Then Fail

```
Connection attempt hangs for 15+ seconds then fails
```

**Check**: Is `WORKER_SELECT_TIMEOUT` less than 15 seconds?

```bash
# Check proxy config
docker compose exec proxy env | grep WORKER_SELECT_TIMEOUT
```

---

## Summary Table

| Parameter | Location | Default | Depends On | Safe Value |
|-----------|----------|---------|------------|------------|
| `HEARTBEAT_INTERVAL` | Worker | 5s | Nothing | Baseline |
| `REDIS_KEY_TTL` | Worker | 60s | Heartbeat | ≥ 2× heartbeat |
| `SHUTDOWN_COMMAND_TTL` | Proxy | 60s | Heartbeat | ≥ 4× heartbeat |
| Reaper Stale Threshold | Proxy (hardcoded) | 30s | Heartbeat | ≥ 3× heartbeat |
| `WORKER_SELECT_TIMEOUT` | Proxy | 5s | HTTP timeout | < 15s |

---

## See Also

- **[Worker Lifecycle](../concepts/worker-lifecycle.md)** — How heartbeats and draining work
- **[Troubleshooting](../operations/troubleshooting.md)** — Debug timing issues
