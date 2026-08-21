# Benchmarks

Two reproducible benchmarks measure what the project optimizes for:

1. **Time-to-first-page** (`time-to-first-page.js`) — how long a client waits
   between asking for a browser and having a page ready. Three baselines:
   - `grid connect()` — a fresh isolated session from the grid;
   - `local launch()` — a browser launched per task on the same machine;
   - `local reused browser` — a context on an already-running local browser,
     the way `@playwright/test` reuses a worker's browser.

   The honest reading: a warm local browser is the latency floor, and the
   grid does not try to beat it. What the numbers show is the price of the
   grid — a few milliseconds of relay overhead for isolation, one shared
   endpoint, and fleet capacity.
2. **Throughput** (`throughput.js`) — complete sessions per second at a fixed
   client concurrency. A session is connect → context → page → goto → close,
   timed end to end. Two modes:
   - **Saturation**: unlimited workers on one machine, showing where a single
     machine tops out end to end.
   - **Scaling**: each worker capped at a fixed CPU budget (`WORKER_CPUS`),
     so adding a worker simulates adding a fixed-size node. Run against 1, 2,
     and 3 workers to show the throughput curve.

Both use a `data:` URL page, so no external network time pollutes the numbers.
There is deliberately no memory benchmark: the project optimizes connect
latency and scale-out, not browser memory use.

## Setup

Requires Node.js 20+ and Docker.

```bash
cd bench
npm ci
npx playwright install --with-deps chromium   # local baselines need a local browser
```

The pinned `playwright` version must match the grid's worker version
(`scripts/check-playwright-version.js` enforces this; the server routes
clients to version-matched workers).

## Run

All commands run from `bench/`. Start every measured sequence from a fresh
stack — `down -v` also removes postgres's anonymous volume, so no session
rows or per-worker counters leak between sequences:

```bash
docker compose down -v
```

**Latency** (1 worker, no CPU cap):

```bash
docker compose up -d --build --scale worker=1
node time-to-first-page.js --expect-workers 1
```

**Throughput, saturation mode** (unlimited workers, concurrency = workers x
MAX_SLOTS, 5 per worker). Run it three times and publish the median:

```bash
docker compose down -v && docker compose up -d --scale worker=3
node throughput.js --sessions 1500 --concurrency 15 --expect-workers 3 --label saturation
```

While a saturation run is in flight, capture per-container CPU in another
shell to attribute the load:

```bash
docker stats --no-stream --format '{{.Name}}\t{{.CPUPerc}}'
```

**Throughput, scaling mode** (fixed-size workers, growing fleet). Fresh stack
per point, three runs per point, publish the median:

```bash
docker compose down -v && WORKER_CPUS=2 docker compose up -d --scale worker=1
node throughput.js --sessions 500  --concurrency 5  --expect-workers 1 --label "1 worker"

docker compose down -v && WORKER_CPUS=2 docker compose up -d --scale worker=2
node throughput.js --sessions 1000 --concurrency 10 --expect-workers 2 --label "2 workers"

docker compose down -v && WORKER_CPUS=2 docker compose up -d --scale worker=3
node throughput.js --sessions 1500 --concurrency 15 --expect-workers 3 --label "3 workers"
```

`WORKER_CPUS` is passed inline on purpose: exporting it would silently cap
the saturation runs too.

Script options: `--endpoint` (default `ws://127.0.0.1:8080`), `--iterations`,
`--warmup`, `--mode all|grid|local|local-reused`, `--expect-workers` for
latency; `--sessions`, `--concurrency`, `--warmup` (sessions, default 2x
concurrency), `--expect-workers`, `--label` for throughput.

## Methodology notes

- `--expect-workers` polls `/v1/capacity` until the fleet has registered and
  refuses to run when `--concurrency` exceeds free slots, so the result
  measures service rate rather than admission-control queueing.
- Throughput runs discard a warmup phase (default 2x concurrency sessions)
  so ramp costs — cold client, first context on each worker, the scheduler
  filling slots — stay out of the measured window.
- The per-session timer includes `browser.close()`: a slot is busy until the
  session is fully torn down, so anything less would overstate capacity.
- The bench stack disables worker recycling (`MAX_LIFETIME_SESSIONS=0`; the
  production default drains a worker after 50 lifetime sessions). With
  recycling on, the throughput number would mostly measure how fast your
  container runtime restarts workers. In a real multi-worker fleet, staggered
  selection spreads recycles out, so the fleet keeps serving while one worker
  restarts.
- The bench stack runs in unauthenticated bootstrap mode (zero API keys),
  like the local compose stack. With authentication enabled the server also
  verifies the key hash on every request — roughly one extra indexed lookup —
  which these numbers do not include.
- The scaling mode exists because stacking unlimited workers on one machine
  cannot show scale-out: they all share the same CPUs, so throughput flattens
  at the machine's limit no matter how many workers run. Capping each worker
  at a fixed CPU budget makes "add a worker" mean "add capacity", which is
  what adding a node does in a real deployment. It stays honest only while
  the machine has spare CPUs for the client, server, and postgres.
- Everything (client, server, workers, postgres) shares one machine in this
  setup, so the processes compete for CPU. That deflates grid numbers rather
  than inflating them: a client on a separate host will only look better.
- Report the hardware next to any published numbers: CPU count, RAM, and
  whether other load was present.

## Results (2026-08-21)

Machine: AMD Ryzen 5 5600 (6 cores / 12 threads), 30 GB RAM, Linux. Client,
server, postgres, and all workers colocated; desktop background load present.
Playwright 1.62.1. Throughput numbers are the median of three runs.

Time-to-first-page (30 iterations):

|                      | min | p50 | p95 | max |
|----------------------|-----|-----|-----|-----|
| grid `connect()`     | 31ms | 35ms | 37ms | 40ms |
| local `launch()`     | 76ms | 86ms | 91ms | 91ms |
| local reused browser | 23ms | 25ms | 27ms | 28ms |

Reading: a fresh isolated grid session costs ~10ms more than a context on an
already-warm local browser, and ~50ms less than launching a browser per task.

Throughput, saturation mode (3 unlimited workers, concurrency 15):
**98 sessions/s** (5,881 sessions/min; runs: 95.2 / 98.0 / 98.9, 4500/4500
completed). Per-container CPU mid-run: each worker ~320%, server 33%,
postgres 33% — the browsers consume the machine; the coordination layer uses
well under one core.

Throughput, scaling mode (`WORKER_CPUS=2`, concurrency = 5 per worker):

| workers | sessions/s | vs 1 worker | session p50 |
|---------|-----------|-------------|-------------|
| 1 | 25.0 | 1.0x | 200ms |
| 2 | 47.2 | 1.9x | 204ms |
| 3 | 74.4 | 3.0x | 200ms |

Throughput grows linearly with workers and per-session latency stays flat.
(Per-run spread was under 4% at every point.)
