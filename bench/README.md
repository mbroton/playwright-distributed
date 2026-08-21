# Benchmarks

Two reproducible benchmarks measure what the project optimizes for:

1. **Time-to-first-page** (`time-to-first-page.js`) — how long a client waits
   between asking for a browser and having a page ready. Compares
   `chromium.connect()` against the grid (warm browser, no launch) with plain
   `chromium.launch()` on the same machine (cold launch, the default everyone
   starts from).
2. **Throughput** (`throughput.js`) — complete sessions per second at a fixed
   client concurrency. Two modes:
   - **Saturation**: unlimited workers on one machine, showing that the
     coordination layer is not the bottleneck (the machine's CPU is).
   - **Scaling**: each worker pinned to a fixed CPU budget (`WORKER_CPUS`),
     so adding a worker simulates adding a fixed-size node. Run against 1, 2,
     and 3 workers to show the throughput curve.

Both use a `data:` URL page, so no external network time pollutes the numbers.
There is deliberately no memory benchmark: the project optimizes connect
latency and scale-out, not browser memory use.

## Setup

```bash
cd bench
npm ci
npx playwright install chromium   # local-launch baseline needs a local browser
```

The pinned `playwright` version must match the grid's worker version
(`scripts/check-playwright-version.js` enforces this).

## Run

All commands below run from `bench/`. Start the grid:

```bash
docker compose -f ../bench/docker-compose.yaml up -d --build --scale worker=1
```

Wait until `curl -s localhost:8080/v1/capacity` reports the expected workers,
then:

```bash
# Latency: grid connect vs local launch
node time-to-first-page.js

# Throughput, saturation mode: how much one machine serves end to end
# (concurrency = workers x MAX_SLOTS, 5 per worker)
docker compose -f ../bench/docker-compose.yaml up -d --scale worker=3
node throughput.js --sessions 600 --concurrency 15 --label "saturation"

# Throughput, scaling mode: fixed-size workers, growing fleet
export WORKER_CPUS=2
docker compose -f ../bench/docker-compose.yaml up -d --scale worker=1
node throughput.js --sessions 150 --concurrency 5  --label "1 worker"
docker compose -f ../bench/docker-compose.yaml up -d --scale worker=2
node throughput.js --sessions 200 --concurrency 10 --label "2 workers"
docker compose -f ../bench/docker-compose.yaml up -d --scale worker=3
node throughput.js --sessions 300 --concurrency 15 --label "3 workers"
```

Between scaling steps, wait until `/v1/capacity` shows the new worker count.

Options: `--endpoint` (default `ws://127.0.0.1:8080`), `--iterations`,
`--warmup`, `--mode grid|local|both` for latency; `--sessions`,
`--concurrency`, `--label` for throughput.

## Methodology notes

- Keep `--concurrency` at or below capacity (workers x `MAX_SLOTS`), so the
  result measures service rate rather than admission-control queueing.
- The bench stack disables worker recycling (`MAX_LIFETIME_SESSIONS=0`; the
  production default drains a worker after 50 lifetime sessions). With
  recycling on, the throughput number would mostly measure how fast your
  container runtime restarts workers. In a real multi-worker fleet, staggered
  selection spreads recycles out, so the fleet keeps serving while one worker
  restarts.
- Everything (client, server, workers, postgres) shares one machine in this
  setup, so the processes compete for CPU. That deflates grid numbers rather
  than inflating them: published numbers include the machine's specs, and a
  client on a separate host will only look better.
- The scaling mode exists because stacking unlimited workers on one machine
  cannot show scale-out: they all share the same CPUs, so throughput flattens
  at the machine's limit no matter how many workers run. Pinning each worker
  to a fixed CPU budget makes "add a worker" mean "add capacity", which is
  what adding a node does in a real deployment. It stays honest only while
  the machine has spare CPUs for the client, server, and postgres.
- Report the hardware next to any published numbers: CPU count, RAM, and
  whether other load was present.

## Results (2026-08-21)

Machine: AMD Ryzen 5 5600 (6 cores / 12 threads), 30 GB RAM, Linux. Client,
server, postgres, and all workers colocated; desktop background load present.
Playwright 1.62.1.

Time-to-first-page (30 iterations):

|                  | min | p50 | p95 | max |
|------------------|-----|-----|-----|-----|
| grid `connect()` | 32ms | 34ms | 38ms | 40ms |
| local `launch()` | 77ms | 83ms | 89ms | 90ms |

Throughput, saturation mode (3 unlimited workers, concurrency 15):
**95 sessions/s** (5,697 sessions/min, 600/600 completed) with the machine at
~95% CPU — the browsers consume the machine; the grid machinery does not.

Throughput, scaling mode (`WORKER_CPUS=2`, concurrency = 5 per worker):

| workers | sessions/s | vs 1 worker | session p50 |
|---------|-----------|-------------|-------------|
| 1 | 25.2 | 1.0x | 198ms |
| 2 | 46.5 | 1.8x | 203ms |
| 3 | 66.6 | 2.6x | 210ms |

Per-session latency stays flat as the fleet grows; the sub-linear tail at 3
workers is the shared client/server/postgres competing for the remaining
cores of a 6-core machine.
