# Control plane server

The server uses these environment variables:

- `DATABASE_URL` is required. It is the PostgreSQL connection string.
  Budget `pool_max_conns + 1` PostgreSQL backends per replica for the hijacked
  capacity-listener connection.
- `LISTEN_ADDR` is optional. Its default value is `:8080`.
- `WORKER_HEARTBEAT_TTL` controls when an available worker becomes stalled.
  Its default is `30s`.
- `SESSION_HEARTBEAT_TTL` controls when a pending or running session expires.
  Its default is `30s`.
- `SESSION_HEARTBEAT_INTERVAL` controls how often a live relay renews its
  session. Its default is `10s`. It must be less than
  `SESSION_HEARTBEAT_TTL`.
- `PENDING_SESSION_TTL` limits how long a claimed session can stay pending.
  Its default is `30s`.
- `STALLED_WORKER_TTL` controls when an idle stalled worker row is removed.
  Its default is `10m`.
- `RESCUER_INTERVAL` controls the base interval between rescuer sweeps. Its
  default is `5s`; each interval has ±20% jitter.
- `MAX_QUEUE_SIZE` limits the admission queue on each server replica. Its
  default is `100`; `0` disables queueing.
- `QUEUE_WAIT_TIMEOUT` limits how long a request can wait in this replica's
  admission queue. Its default is `30s`.
- `MAX_LIFETIME_SESSIONS` drains a worker after this many claims. Its default
  is `50`; `0` disables recycling.
- `DEFAULT_BROWSER_TYPE` selects the browser for `GET /` when the client does
  not set `?browser=`. Its default is `chromium`. Valid values are `chromium`,
  `firefox`, and `webkit`.
- `WORKER_DIAL_TIMEOUT` is the total time limit for a worker WebSocket dial.
  Its default is `10s`.
- `RELAY_WRITE_TIMEOUT` limits each relay write. Its default is `30s`.
- `RELAY_PING_INTERVAL` controls how often the relay pings each peer. Its
  default is `20s`. It must be less than `RELAY_PONG_TIMEOUT`.
- `RELAY_PONG_TIMEOUT` controls how long a relay can receive no data or pong.
  Its default is `60s`.
- `SHUTDOWN_GRACE_PERIOD` gives active relays time to finish after shutdown
  starts. Its default is `20s`. The server then closes each remaining relay
  with WebSocket code `1001`.

The admission queue is in memory and belongs to one server replica. The
`queued` capacity field reports only that replica's queue depth. Database
notifications wake queues on all replicas, and a one-second polling fallback
keeps admission working while a notification listener reconnects.

The `serve` command applies database migrations before it starts the HTTP
server. API key commands open the database but do not apply migrations.

## WebSocket data plane

The WebSocket routes are not part of the OpenAPI document.

```text
GET /?browser=chromium&token=pwd_...
    -> claim a worker and create a session
    -> dial the worker
    -> start the session
    -> upgrade the client

GET /sessions/{id}?token=pwd_...
    -> load a pending session
    -> dial its worker
    -> start the session
    -> upgrade the client
```

Both routes also accept `Authorization: Bearer pwd_...`. The query token is
for stock Playwright clients that cannot set an authorization header. Request
logs contain only the URL path and never contain the query string.

The optional `browser` value is `chromium`, `firefox`, or `webkit`. A client
user agent such as `Playwright/1.62.1` selects a worker whose version starts
with `1.62.`. An explicit attach rejects a client with a different major or
minor version.

The relay forwards `User-Agent` and `x-playwright-*` request headers to the
worker. It does not forward authorization, cookies, or query tokens. It also
sends `x-pwd-session-id` with the session UUID.

`DELETE /v1/sessions/{id}` completes a pending or running session. The relay
owner sees the change on its next session heartbeat and closes both WebSocket
peers with code `1001`. The maximum normal detection delay is one
`SESSION_HEARTBEAT_INTERVAL`.

Manage API keys with the `apikey` command:

```text
server apikey create --name deployment
server apikey list
server apikey revoke --id <uuid>
```

## Authentication bootstrap mode

**A server with zero active API keys serves the entire control plane without
authentication.** Create the first API key to lock the running server and
require a bearer token for all control-plane routes. The server stays locked
for its process lifetime, even if you later revoke every key. Restart the
server to enter bootstrap mode again when the database has zero active keys.
