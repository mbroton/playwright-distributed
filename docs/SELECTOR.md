# Worker Selection Strategy

This document explains how the proxy selects workers from the available pool and why this strategy helps maintain system stability.

## Overview

The worker selector in `proxy/internal/redis/selector.lua` implements a **lifetime-first selection strategy** designed to prevent multiple workers from reaching their session limit simultaneously, which would cause multiple restarts at the same time and temporarily reduce cluster capacity.

## Strategy Details

### Primary Goal: Stagger Worker Restarts

Instead of distributing load evenly across all workers (which causes them to reach their limits at the same time), we intentionally "push" one worker to its limit first while keeping others behind. This ensures only one worker restarts at a time.

### Worker Discovery

Workers are pre-registered in the `cluster:active_connections` hash on startup with an initial value of 0. This ensures all workers are visible to the selection algorithm, including those with zero active connections.

### Selection Algorithm

1. **Early exit**: If no workers are available in the cluster, return `nil` immediately to prevent errors.

2. **Filter eligible workers** based on:
   - Status is "available"
   - Active connections < `MAX_CONCURRENT_SESSIONS`
   - Lifetime connections < `MAX_LIFETIME_SESSIONS`
   - Recent heartbeat (within last 60 seconds)

3. **Calculate adaptive safety margin**:
   ```
   margin = max(1, floor(MAX_LIFETIME_SESSIONS / total_workers))
   ```

4. **Primary selection** (respecting safety margin):
   - Among workers with `lifetime < (MAX_LIFETIME_SESSIONS - margin)`
   - Select worker with **highest lifetime** connections
   - Tie-break with **lowest active** connections

5. **Fallback selection** (respects hard limit):
   - If no workers pass the margin check
   - Among workers with `lifetime + 1 <= MAX_LIFETIME_SESSIONS`
   - Select worker with **highest lifetime** connections
   - Tie-break with **lowest active** connections

## Example Scenarios

### Scenario 1: Normal Operation
- **Setup**: 4 workers, `MAX_LIFETIME_SESSIONS = 20`
- **Margin**: `max(1, floor(20/4)) = 5`
- **Current state**:
  ```
  Worker A: lifetime=18, active=2  # Within margin (18 >= 15), not selectable
  Worker B: lifetime=12, active=1  # Best choice (highest lifetime under margin)
  Worker C: lifetime=8,  active=0
  Worker D: lifetime=3,  active=1
  ```
- **Result**: Worker B is selected
- **Rationale**: Worker A is close to its limit, so we avoid it. Worker B has done the most work among the remaining candidates.

### Scenario 2: Small Cluster with High Load
- **Setup**: 2 workers, `MAX_LIFETIME_SESSIONS = 10`
- **Margin**: `max(1, floor(10/2)) = 5`
- **Current state**:
  ```
  Worker A: lifetime=8, active=3   # Within margin (8 >= 5), not selectable
  Worker B: lifetime=7, active=2   # Within margin (7 >= 5), not selectable
  ```
- **Fallback triggered**: Both workers are within margin
- **Result**: Worker A is selected (highest lifetime, and 8+1 <= 10)
- **Rationale**: System prioritizes progress over perfect staggering when necessary, but never exceeds the hard limit.

### Scenario 3: Development Environment
- **Setup**: 1 worker, `MAX_LIFETIME_SESSIONS = 4`
- **Margin**: `max(1, floor(4/1)) = 4`
- **Current state**:
  ```
  Worker A: lifetime=2, active=0   # Under margin (2 < 0), selectable
  ```
- **Result**: Worker A is selected
- **Rationale**: With only one worker, we can't stagger, but the margin prevents issues at the boundary.

### Scenario 4: Edge Case - Worker at Limit
- **Setup**: 3 workers, `MAX_LIFETIME_SESSIONS = 10`
- **Margin**: `max(1, floor(10/3)) = 3`
- **Current state**:
  ```
  Worker A: lifetime=9, active=1   # At limit-1, can be selected by fallback (9+1 <= 10)
  Worker B: lifetime=7, active=2   # Within margin (7 >= 7), not selectable in primary
  Worker C: lifetime=5, active=0   # Best primary choice (highest under margin)
  ```
- **Result**: Worker C is selected (primary selection)
- **Rationale**: Primary selection prefers Worker C. If Worker C were unavailable, fallback would select Worker A but never Worker B at lifetime=10.

## Benefits

### 1. **Predictable Capacity**
- Only one worker restarts at a time
- Cluster maintains `(N-1)/N` capacity during restarts instead of dropping significantly

### 2. **Load Distribution**
- Active connection tie-breaker prevents overloading busy workers
- System remains responsive under concurrent load

### 3. **Adaptive Behavior**
- Margin scales with cluster size automatically
- Works efficiently for both small dev clusters and large production deployments

### 4. **Performance**
- O(N) algorithm - single pass through workers

### 5. **Robustness**
- Guards against edge cases (empty worker pool, division by zero)
- Guarantees compliance with hard lifetime limits
- All workers visible regardless of current active connection count

## Configuration Impact

The selection strategy interacts with these configuration values:

- **`MAX_CONCURRENT_SESSIONS`**: Hard limit on concurrent connections per worker
- **`MAX_LIFETIME_SESSIONS`**: Triggers worker restart and drives the selection preference
- **Worker count**: Automatically detected and used to calculate the safety margin

## Monitoring

To observe the strategy in action, monitor:

1. **Lifetime connection distribution** across workers
2. **Restart timing** - should see staggered restarts, not simultaneous ones
3. **Active connection spikes** during high load periods

The strategy should show a "sawtooth" pattern in lifetime connections over time, with one worker climbing to the limit, restarting (dropping to 0), while others remain at intermediate levels. 