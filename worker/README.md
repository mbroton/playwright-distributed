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
