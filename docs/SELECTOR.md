# Worker Selection Strategy

This document explains how the proxy chooses a worker and why the selector is intentionally biased toward both healthy load distribution and staggered worker recycling.

## Overview

The selector in `proxy/internal/redis/selector.lua` now uses **balanced routing with restart bias**.

That means:

- route only to healthy, eligible workers
- prefer the least-loaded eligible worker first
- keep an allocated-session safety margin so workers do not all hit their recycle limit at the same time
- stay deterministic so behavior is predictable and easy to test

The selector still runs in Redis/Lua because selection and counter increment must stay atomic.

The selector uses `cluster:allocated_sessions` as its optimistic lifetime budget counter. Worker drain decisions use `cluster:successful_sessions` instead. That split keeps async rollback from draining a healthy worker one session early.

## Eligibility Rules

A worker is eligible only if all of these are true:

- browser type matches the requested browser
- status is `available`
- active connections are below `MAX_CONCURRENT_SESSIONS`
- allocated sessions are below `MAX_LIFETIME_SESSIONS`
- `lastHeartbeat` is within `SELECTOR_FRESHNESS_TIMEOUT`
- the worker metadata needed for routing is present and well-formed
- the worker is not excluded for the current connection attempt

The selector ignores orphan counter entries that no longer have a valid worker record. Cleanup of those stale counters remains the reaper's job.

## Selection Algorithm

The selector uses two tiers.

### 1. Tier 1: Headroom Tier

Workers in this tier satisfy:

```text
allocated < MAX_LIFETIME_SESSIONS - margin
```

The margin is computed from the number of **eligible** workers for the requested browser:

```text
margin = max(1, floor(MAX_LIFETIME_SESSIONS / eligible_workers))
```

This keeps workers with enough allocated headroom in the preferred pool and prevents the selector from pushing every worker toward the recycle edge at the same rate.

### 2. Tier 2: Fallback Tier

If Tier 1 is empty, the selector falls back to any remaining eligible worker that still stays under the hard lifetime limit for one more session:

```text
allocated + 1 <= MAX_LIFETIME_SESSIONS
```

This fallback keeps the system making progress under pressure without violating the hard limit.

## Ranking Inside a Tier

Within either tier, workers are ranked in this order:

1. lower active connections
2. higher allocated sessions
3. older `startedAt`
4. lexicographically smaller worker ID

Why this order:

- lower active connections keeps routing load-aware
- higher allocated sessions keep the restart-staggering bias
- older `startedAt` breaks ties in a stable way
- worker ID is the final deterministic tiebreaker

## Why This Replaced Pure Lifetime-First Routing

The previous lifetime-first strategy did stagger restarts, but it over-prioritized lifetime even when another healthy worker was less loaded.

The new strategy keeps the useful part of the old behavior while fixing that imbalance:

- load awareness comes first inside a tier
- restart bias still exists through the allocated-session safety margin and the allocated-session tiebreak
- margin calculation now depends on eligible workers, not raw counter hashes

## Failure Handling Boundary

The selector itself only chooses workers and increments counters atomically. Failure recovery around selection happens in the proxy handler.

For one connection attempt, the proxy may exclude a selected worker and ask the selector for another one when a selected worker fails fast before the handoff succeeds, for example:

- invalid worker endpoint
- immediate backend dial failure
- selected worker disappears before a usable handoff can be completed

Those exclusions are request-scoped only. They are not a cluster-wide blacklist.

The proxy does **not** use selector retries for:

- backend dial timeout
- client upgrade failure after Gorilla starts the upgrade path

Those remain terminal handoff failures for that client connection.

## Configuration Impact

The selector depends on these values:

- `MAX_CONCURRENT_SESSIONS`
- `MAX_LIFETIME_SESSIONS`
- `SELECTOR_FRESHNESS_TIMEOUT`

`SELECTOR_FRESHNESS_TIMEOUT` is intentionally explicit now. The selector no longer relies on a hardcoded heartbeat age threshold.

`MAX_LIFETIME_SESSIONS` still refers to successful sessions at the worker lifecycle level. The selector uses `allocated_sessions` conservatively against that same limit so in-flight handoffs cannot oversubscribe the worker.

## Operational Expectations

If the selector is working correctly, you should see:

- draining and stale workers stop receiving new sessions
- lower-active healthy workers preferred over busier ones
- allocated-session counts rise unevenly enough that worker recycling is staggered rather than synchronized
- fast bad-worker failures retried against other workers within the selection timeout when possible

The system should still show a sawtooth-style allocated-session pattern over time, but with healthier per-request load distribution than a pure lifetime-first policy.

## Known Scale Tradeoff

The current selector still scans Redis worker state in O(N) per selection attempt. That is intentional for the current expected pool size because it keeps selection logic deterministic and atomic inside Lua. If the worker fleet or the number of queued clients grows substantially, this design will need a different data structure rather than incremental tuning.
