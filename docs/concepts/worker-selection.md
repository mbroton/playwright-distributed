# Worker Selection

When you connect to the grid, the proxy must choose which worker handles your request. This isn't random—it's a carefully designed algorithm that keeps the cluster stable.

---

## The Problem

Imagine 4 workers, each with a lifetime limit of 50 sessions. With naive round-robin:

```
Time 0:   Worker A: 0    Worker B: 0    Worker C: 0    Worker D: 0
Time 10:  Worker A: 12   Worker B: 13   Worker C: 12   Worker D: 13
Time 20:  Worker A: 25   Worker B: 25   Worker C: 25   Worker D: 25
Time 30:  Worker A: 38   Worker B: 37   Worker C: 38   Worker D: 37
Time 40:  Worker A: 50   Worker B: 50   Worker C: 50   Worker D: 50
          ↓              ↓              ↓              ↓
          RESTART        RESTART        RESTART        RESTART
```

**All four workers restart simultaneously.** During this window, the cluster has zero capacity.

---

## The Solution: Lifetime-First Selection

Instead of distributing load evenly, we intentionally push one worker to its limit first:

```
Time 0:   Worker A: 0    Worker B: 0    Worker C: 0    Worker D: 0
Time 10:  Worker A: 35   Worker B: 10   Worker C: 4    Worker D: 1
Time 12:  Worker A: 50   (restarts)
          ↓
Time 13:  Worker A: 0    Worker B: 35   Worker C: 10   Worker D: 5
Time 25:  Worker A: 15   Worker B: 50   (restarts)
                         ↓
Time 26:  Worker A: 20   Worker B: 0    Worker C: 35   Worker D: 10
```

**Only one worker restarts at a time.** Cluster maintains (N-1)/N capacity.

---

## The Algorithm

### Step 1: Filter Eligible Workers

A worker is eligible if:
- `status = "available"` (not draining or dead)
- `activeConnections < MAX_CONCURRENT_SESSIONS`
- `lifetimeConnections < MAX_LIFETIME_SESSIONS`
- `lastHeartbeat` within the last 60 seconds

### Step 2: Calculate Safety Margin

```
margin = max(1, floor(MAX_LIFETIME_SESSIONS / total_workers))
```

The margin creates a buffer zone before the lifetime limit. Workers in this zone are avoided unless necessary.

**Examples:**
- 4 workers, limit 50: margin = 12
- 2 workers, limit 50: margin = 25
- 10 workers, limit 100: margin = 10

### Step 3: Primary Selection

Among workers with `lifetime < (MAX_LIFETIME_SESSIONS - margin)`:
1. Select the worker with the **highest lifetime** (push it toward the limit)
2. Tie-breaker: lowest **active connections** (spread current load)

### Step 4: Fallback Selection

If no workers pass the margin check (all are in the "danger zone"):
1. Select from workers where `lifetime + 1 <= MAX_LIFETIME_SESSIONS`
2. Same preference: highest lifetime, then lowest active

This ensures we can still serve requests even when all workers are near their limits.

---

## Visual Example

```
MAX_LIFETIME_SESSIONS = 20
Workers = 4
Margin = 5

                    Safe Zone        Margin Zone
                    (0-14)           (15-19)          Limit (20)
                    ├────────────────┼────────────────┤
                    │                │                │
Worker A: ████████████████████       │                │  lifetime=16 ← In margin
Worker B: ████████████               │                │  lifetime=12 ← SELECTED
Worker C: ████████                   │                │  lifetime=8
Worker D: ███                        │                │  lifetime=3
```

Worker B is selected because:
- It's in the safe zone (below margin)
- It has the highest lifetime in the safe zone

---

## Why This Works

### 1. Predictable Capacity

With staggered restarts, you always have at least `(N-1)/N` workers available:
- 4 workers: 75% capacity during restarts
- 10 workers: 90% capacity during restarts

### 2. Automatic Adaptation

The margin scales with cluster size:
- Small cluster (2 workers): Larger margin gives more buffer
- Large cluster (20 workers): Smaller margin, finer control

### 3. No Coordination Required

Workers don't need to communicate with each other. The proxy makes all decisions based on Redis state.

### 4. Efficient

The selection runs in O(N) time—single pass through workers.

---

## Configuration Impact

| Setting | Effect on Selection |
|---------|---------------------|
| `MAX_CONCURRENT_SESSIONS` | Hard limit on how many connections per worker |
| `MAX_LIFETIME_SESSIONS` | When workers restart; affects margin calculation |
| Worker count | More workers = smaller margin = tighter staggering |

### Tuning for Your Workload

**High-throughput, short sessions:**
- Lower `MAX_LIFETIME_SESSIONS` (e.g., 30)
- More frequent restarts keep memory fresh
- Margin stays small, tight staggering

**Long-running sessions:**
- Higher `MAX_LIFETIME_SESSIONS` (e.g., 100)
- Fewer restarts, less disruption
- Larger margin, more buffer

---

## Monitoring Selection

### Check Lifetime Distribution

```bash
redis-cli HGETALL cluster:lifetime_connections
```

You should see a "staircase" pattern:
```
chromium:worker1 -> 45
chromium:worker2 -> 30
chromium:worker3 -> 15
chromium:worker4 -> 5
```

### Watch the Sawtooth

Over time, lifetime counts should show a sawtooth pattern:

```
Lifetime
   50 │    ╱╲        ╱╲        ╱╲
      │   ╱  ╲      ╱  ╲      ╱  ╲
   25 │  ╱    ╲    ╱    ╲    ╱    ╲
      │ ╱      ╲  ╱      ╲  ╱      ╲
    0 │╱────────╲╱────────╲╱────────╲───── Time
        Worker A   Worker B   Worker C
```

### Signs of Problems

| Pattern | Indicates |
|---------|-----------|
| All workers at same lifetime | Selection not working, check proxy logs |
| Multiple simultaneous restarts | Bug or race condition |
| Workers never reaching limit | Traffic too low, or lifetime too high |

---

## Edge Cases

### Single Worker

With one worker, there's no staggering possible. The worker simply restarts when it hits the limit, causing a brief outage.

**Recommendation:** Run at least 2 workers in production.

### All Workers in Margin Zone

When all workers are within the margin of the limit:

```
margin = 5, limit = 20

Worker A: lifetime=18  ← In margin
Worker B: lifetime=17  ← In margin
Worker C: lifetime=16  ← In margin
```

Fallback kicks in: Select Worker A (highest lifetime, won't exceed limit).

### All Workers at Limit

If somehow all workers are at exactly the limit:

```
Worker A: lifetime=20  ← At limit
Worker B: lifetime=20  ← At limit
```

No worker is selectable. Proxy waits (up to `WORKER_SELECT_TIMEOUT`) for a worker to restart and become available.

---

## Implementation Details

The selection logic lives in a Lua script (`selector.lua`) that runs atomically in Redis. This ensures:
- No race conditions between concurrent selections
- Consistent counter increments
- Fast execution (sub-millisecond)

---

## Next

- **[Worker Lifecycle](worker-lifecycle.md)** — How workers start, run, and restart
- **[Timing Configuration](../configuration/timing.md)** — Heartbeats and timeouts
