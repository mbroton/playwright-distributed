# Control plane server

The server uses these environment variables:

- `DATABASE_URL` is required. It is the PostgreSQL connection string.
- `LISTEN_ADDR` is optional. Its default value is `:8080`.
- `WORKER_HEARTBEAT_TTL` controls when an available worker becomes stalled.
  Its default is `30s`.
- `SESSION_HEARTBEAT_TTL` controls when a pending or running session expires.
  Its default is `30s`.
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

The admission queue is in memory and belongs to one server replica. The
`queued` capacity field reports only that replica's queue depth. Database
notifications wake queues on all replicas, and a one-second polling fallback
keeps admission working while a notification listener reconnects.

The `serve` command applies database migrations before it starts the HTTP
server. API key commands open the database but do not apply migrations.

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
