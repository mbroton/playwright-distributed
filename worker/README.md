# Worker

A worker is a container that keeps one browser running and serves up to
`MAX_SLOTS` concurrent sessions on it, each in its own isolated context. It
registers itself with the server on start; configuration is environment
variables (see [`.env.example`](.env.example)).

## Tuning `MAX_SLOTS`

`MAX_SLOTS` (default 5) caps how many sessions one worker serves at once.
The right value depends on what the sessions do:

- **Busy tasks** (fast pages, constant actions): CPU is the limit. Use about
  **2 slots per CPU** available to the worker. Beyond that, throughput stays
  flat and every session just gets slower.
- **Tasks that wait on slow pages**: CPU sits idle during page loads, so the
  same worker can hold 2–3× more slots. Memory becomes the limit — each
  active session adds to the browser's footprint, so raise the value in
  steps and watch the worker's memory.

When in doubt, keep the default and add workers instead of raising
`MAX_SLOTS`: capacity grows the same either way, and each worker bounds how
many sessions a browser crash or recycle can take down with it.

## Scaling beyond one machine

The grid has two kinds of parts: one **server with its PostgreSQL** (the
control plane, and the relay every session flows through) and any number of
**workers**, which can run on any host that can reach the server. Workers
are disposable: start one and it registers itself, stop one and it drains.

```
clients ──► server + postgres (host A) ──┬──► worker, worker   (host B)
                                         ├──► worker, worker   (host C)
                                         └──► worker           (host D)
```

**1. Open the server to the network.** The quick-start compose file binds the
server to `127.0.0.1:8080`. Workers on other hosts need it reachable, so
change the mapping to `"8080:8080"` (or put it behind your own proxy) and
create an API key — an open bootstrap-mode server on a network is not
something you want:

```bash
docker compose exec server server apikey create --name grid
```

**2. Make each worker reachable from the server.** The server connects *to*
workers, at the address they advertise: `ws://PRIVATE_HOSTNAME:PORT/`. The
default hostname is the container's own, which means nothing on another
host, so on a worker host set `PRIVATE_HOSTNAME` to an address the server
can dial and publish the port. A worker-only compose file needs the seccomp
profile next to it, same as the quick start:

```yaml
services:
  worker:
    image: ghcr.io/mbroton/playwright-distributed/worker:latest
    init: true
    security_opt:
      - seccomp=./worker/seccomp_profile.json
    shm_size: "1gb"
    stop_grace_period: 60s
    ports:
      - "3131:3131"
    environment:
      - SERVER_URL=http://host-a.internal:8080
      - WORKER_API_KEY=${WORKER_API_KEY}
      - PRIVATE_HOSTNAME=host-b.internal
      - PORT=3131
      - BROWSER_TYPE=chromium
      - MAX_SLOTS=5
      - DRAIN_TIMEOUT=30
    restart: unless-stopped
```

For more workers on the same host, copy the block with a different `PORT`
and port mapping. A worker serves one browser type; run separate workers
for Firefox or WebKit, and clients pick one with `?browser=firefox`.

**3. Size a host.** The `MAX_SLOTS` rule above applies per host as well:
about 2 slots per CPU for busy tasks, more when sessions mostly wait on
pages. Split the total into workers of `MAX_SLOTS` each — an 8-CPU host is
about 16 busy slots, so three workers at the default of 5. Give each host
memory to match; a browser's footprint grows with every open session.

**4. Know when to add workers.** `GET /v1/capacity` reports free slots per
browser type and the queue depth. When every slot is busy, new sessions wait
in the queue up to `QUEUE_WAIT_TIMEOUT` (30s by default) and get a 429 once
the queue is full (`MAX_QUEUE_SIZE`, 100). A queue that stays above zero, or
any 429s, means the grid needs more workers — start them anywhere.

**5. Plan for failures.** Losing a worker host loses only the sessions on it;
the rest of the grid keeps serving and the server hands out the remaining
slots. The server and its database are the one part every session depends
on — a session's traffic flows through the relay, so a server outage ends
every live session. Keep that host reliable and back up its volume; treat
worker hosts as cattle.
