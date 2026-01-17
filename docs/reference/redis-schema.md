# Redis Schema

Technical reference for Redis data structures used by playwright-distributed.

---

## Overview

playwright-distributed uses Redis for:
- Worker registry and discovery
- Connection counting
- Shutdown command delivery
- Health tracking (heartbeats)

---

## Key Patterns

### Worker Registry

**Pattern**: `worker:{browserType}:{workerId}`

**Type**: Hash

**Fields**:

| Field | Type | Description |
|-------|------|-------------|
| `browserType` | string | `chromium`, `firefox`, or `webkit` |
| `wsEndpoint` | string | WebSocket URL for proxy to connect |
| `status` | string | `available` or `draining` |
| `lastHeartbeat` | integer | Unix timestamp of last heartbeat |

**Example**:

```
worker:chromium:abc123
├── browserType: "chromium"
├── wsEndpoint: "ws://worker:3131/playwright/chromium/..."
├── status: "available"
└── lastHeartbeat: 1699999999
```

**TTL**: Set by worker (`REDIS_KEY_TTL`), refreshed on each heartbeat.

**Commands**:

```bash
# Get all fields
HGETALL worker:chromium:abc123

# Get specific field
HGET worker:chromium:abc123 status

# List all workers
KEYS "worker:*:*"

# List workers by browser type
KEYS "worker:chromium:*"
```

---

### Active Connections Counter

**Pattern**: `cluster:active_connections`

**Type**: Hash

**Fields**: `{browserType}:{workerId}` → count

**Example**:

```
cluster:active_connections
├── chromium:abc123: 3
├── chromium:def456: 5
└── firefox:ghi789: 2
```

**Commands**:

```bash
# Get all active connections
HGETALL cluster:active_connections

# Get specific worker's count
HGET cluster:active_connections chromium:abc123

# Increment (done by selector.lua)
HINCRBY cluster:active_connections chromium:abc123 1

# Decrement (done by proxy on disconnect)
HINCRBY cluster:active_connections chromium:abc123 -1
```

---

### Lifetime Connections Counter

**Pattern**: `cluster:lifetime_connections`

**Type**: Hash

**Fields**: `{browserType}:{workerId}` → count

**Example**:

```
cluster:lifetime_connections
├── chromium:abc123: 47
├── chromium:def456: 23
└── firefox:ghi789: 15
```

This counter is monotonically increasing and never decremented. When it reaches `MAX_LIFETIME_SESSIONS`, the worker is scheduled for restart.

**Commands**:

```bash
# Get all lifetime counts
HGETALL cluster:lifetime_connections

# Get specific worker's lifetime
HGET cluster:lifetime_connections chromium:abc123
```

---

### Shutdown Commands

**Pattern**: `worker:cmd:{browserType}:{workerId}`

**Type**: String

**Value**: `"shutdown"`

**TTL**: `SHUTDOWN_COMMAND_TTL` (default 60 seconds)

**Example**:

```
worker:cmd:chromium:abc123 = "shutdown"
TTL: 60 seconds
```

Workers check for this key during each heartbeat. If found, they enter drain mode.

**Commands**:

```bash
# Check for shutdown command
GET worker:cmd:chromium:abc123

# Send shutdown command (done by proxy)
SET worker:cmd:chromium:abc123 "shutdown" EX 60

# List pending commands
KEYS "worker:cmd:*"
```

---

## Lua Scripts

### Selector Script (`selector.lua`)

**Purpose**: Atomically select a worker and increment counters.

**Input**:
- `KEYS[1]`: Active connections hash key
- `KEYS[2]`: Lifetime connections hash key
- `ARGV[1]`: Browser type
- `ARGV[2]`: MAX_CONCURRENT_SESSIONS
- `ARGV[3]`: MAX_LIFETIME_SESSIONS
- `ARGV[4]`: Current timestamp

**Output**: Worker's WebSocket endpoint or `nil`

**Algorithm**:
1. Find all workers matching browser type
2. Filter by status, heartbeat, and limits
3. Calculate safety margin
4. Select worker with highest lifetime (under margin)
5. Fall back to any eligible worker if needed
6. Increment active and lifetime counters
7. Return selected worker's endpoint

### Reaper Script (`reaper.lua`)

**Purpose**: Clean up stale worker records.

**Input**:
- `ARGV[1]`: Current timestamp
- `ARGV[2]`: Stale threshold (seconds)

**Output**: Number of reaped workers

**Algorithm**:
1. Find all worker keys
2. Check each worker's `lastHeartbeat`
3. If `(now - lastHeartbeat) > threshold`:
   - Delete worker key
   - Delete from active connections hash
   - Delete from lifetime connections hash
4. Return count of deleted workers

---

## Data Flow

### Worker Registration

```
Worker starts
    │
    ▼
HSET worker:chromium:abc123
    browserType chromium
    wsEndpoint ws://...
    status available
    lastHeartbeat <now>
    │
    ▼
HSET cluster:active_connections
    chromium:abc123 0
    │
    ▼
HSET cluster:lifetime_connections
    chromium:abc123 0
    │
    ▼
EXPIRE worker:chromium:abc123 60
```

### Heartbeat

```
Every HEARTBEAT_INTERVAL seconds:
    │
    ▼
HSET worker:chromium:abc123
    lastHeartbeat <now>
    │
    ▼
EXPIRE worker:chromium:abc123 60
    │
    ▼
GET worker:cmd:chromium:abc123
    │
    ├── nil → continue
    └── "shutdown" → enter drain mode
```

### Connection Established

```
Client connects
    │
    ▼
EVALSHA selector.lua
    │
    ├── Returns endpoint
    │   ├── HINCRBY active +1
    │   └── HINCRBY lifetime +1
    │
    └── Returns nil → 503 error
```

### Connection Closed

```
Client disconnects
    │
    ▼
HINCRBY cluster:active_connections
    chromium:abc123 -1
    │
    ▼
Check if lifetime >= MAX_LIFETIME
    │
    ├── No → done
    └── Yes → SET worker:cmd:chromium:abc123 "shutdown" EX 60
```

### Worker Shutdown

```
Worker receives shutdown command
    │
    ▼
HSET worker:chromium:abc123
    status draining
    │
    ▼
Wait for active connections to close
    │
    ▼
DEL worker:chromium:abc123
HDEL cluster:active_connections chromium:abc123
HDEL cluster:lifetime_connections chromium:abc123
    │
    ▼
Exit process
```

---

## Monitoring Redis

### Key Counts

```bash
# Total keys
DBSIZE

# Worker count
KEYS "worker:*:*" | wc -l

# By browser type
KEYS "worker:chromium:*" | wc -l
KEYS "worker:firefox:*" | wc -l
KEYS "worker:webkit:*" | wc -l
```

### Memory Usage

```bash
# Overall memory
INFO memory

# Memory per key (sampling)
MEMORY USAGE worker:chromium:abc123
```

### Check for Orphans

```bash
# Active connections with no corresponding worker
HKEYS cluster:active_connections | while read key; do
    EXISTS worker:$key || echo "Orphan: $key"
done
```

---

## Backup and Recovery

### Export Data

```bash
# Create RDB snapshot
BGSAVE

# Or AOF rewrite
BGREWRITEAOF
```

### Recovery Notes

If Redis data is lost:
1. Workers re-register on next heartbeat (within seconds)
2. Active/lifetime counters reset to 0
3. No manual intervention needed

This means Redis persistence is optional for playwright-distributed.

---

## See Also

- **[Worker Selection](../concepts/worker-selection.md)** — Selection algorithm details
- **[Worker Lifecycle](../concepts/worker-lifecycle.md)** — How workers use Redis
- **[Configuration](../configuration/index.md)** — Redis-related settings
