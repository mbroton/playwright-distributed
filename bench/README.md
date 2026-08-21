# Benchmarks

Reproducible benchmarks for the grid:

1. **Time-to-first-page** (`time-to-first-page.js`) — time from requesting a
   browser to a page being ready for use. Three variants:
   - `grid connect()` — a fresh isolated session from the grid;
   - `local launch()` — a browser launched per task on the same machine;
   - `local reused browser` — a new context on an already-running local
     browser, the way `@playwright/test` reuses a worker's browser.

   The two local variants bound the comparison: `local reused browser` is
   the latency floor on the same machine, and the difference between it and
   `grid connect()` is the grid's relay and scheduling overhead.
2. **Throughput** (`throughput.js`) — complete sessions per second at a fixed
   client concurrency. A session is connect → context → page → goto → close,
   timed end to end. Two modes:
   - **Saturation**: CPU-uncapped workers on one machine, showing where a
     single machine tops out end to end.
   - **Scaling**: each worker capped at a fixed CPU budget (`WORKER_CPUS`),
     so adding a worker simulates adding a fixed-size node. Run against 1, 2,
     and 3 workers to show the throughput curve.

Both use a `data:` URL page, so no external network time pollutes the
numbers.

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

**Throughput, saturation mode** (CPU-uncapped workers, concurrency = workers
x MAX_SLOTS, 5 per worker). Run it three times and take the median:

```bash
docker compose down -v && docker compose up -d --scale worker=3
node throughput.js --sessions 1500 --concurrency 15 --expect-workers 3 --label saturation
```

While a saturation run is in flight, capture per-container CPU in another
shell to attribute the load, and the bench client's own CPU (it is a single
Node process and can become the ceiling on small machines):

```bash
docker stats --no-stream --format '{{.Name}}\t{{.CPUPerc}}'
ps -o %cpu= -p "$(pgrep -fn throughput.js)"
curl -s localhost:8080/v1/capacity   # "queued" should stay at or near 0
```

**Throughput, scaling mode** (fixed-size workers, growing fleet). Fresh stack
per point, three runs per point, take the median:

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

- `--expect-workers` polls `/v1/capacity` until *exactly* that many idle
  workers of the benchmarked browser have registered (extra or leftover
  workers fail the gate too — hence the fresh stack per point), and refuses
  to run when `--concurrency` exceeds free slots. It does not check the
  concurrency-equals-capacity rule below; that is on the operator.
- Concurrency must equal capacity (workers x MAX_SLOTS) in scaling runs, not
  just stay below it: the scheduler deliberately packs the busiest worker
  first (that staggers recycling in production), so at partial concurrency
  added workers sit idle and the curve looks flat even though capacity grew.
- Start each point from a fresh stack (`down -v`); the short warmup phase
  (default 2x concurrency sessions, discarded) additionally keeps cold-client
  and first-context costs out of the measured window.
- Sessions are isolated browser contexts, not browser processes: a worker
  multiplexes up to MAX_SLOTS sessions onto one browser process, which is the
  same isolation level as the `local reused browser` variant. Keep this in
  mind when comparing against per-session-process designs.
- The per-session timer includes `browser.close()`: a slot is busy until the
  session is fully torn down, so anything less would overstate capacity.
- The bench stack disables worker recycling (`MAX_LIFETIME_SESSIONS=0`; the
  production default drains a worker after 50 lifetime sessions). With
  recycling on, the throughput number would also include worker restart time,
  which depends on the container runtime, not on the grid.
- The bench stack runs in unauthenticated bootstrap mode (zero API keys),
  like the local compose stack. With authentication enabled the server also
  verifies the key hash on every request (an indexed lookup, plus a last-used
  timestamp update throttled to once per minute), which adds a small
  per-request cost. To benchmark an authenticated grid:
  `--endpoint 'ws://host:8080/?token=pwd_...'`.
- Workers sharing one machine share its CPUs, so adding workers there cannot
  show scale-out: throughput flattens at the machine's limit no matter how
  many run. The scaling mode caps each worker at a fixed CPU budget
  (`WORKER_CPUS`) so that "add a worker" means "add capacity", as adding a
  node does in a real deployment. The simulation holds only while the machine
  keeps spare CPUs for the client, server, and postgres.
- Results depend on where the bench client runs: on the same machine as the
  grid it competes with the browsers for CPU and lowers grid numbers. For
  representative results, run the client on a separate machine.
- Record the hardware together with any results: CPU count, RAM, and whether
  other load was present.
