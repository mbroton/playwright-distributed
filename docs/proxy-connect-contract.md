# Proxy Connect Contract

This document records the proxy connect contract and the decisions behind it. The README explains how to use the proxy. This file explains why the proxy behaves the way it does.

## Purpose

The proxy has one job during connection setup:

1. Validate the incoming client request.
2. Wait for an eligible worker.
3. Connect to that worker.
4. Upgrade the client connection.
5. Relay traffic between the client and the worker.

The contract below keeps that flow predictable for both clients and maintainers.

## Supported Client Contract

The proxy exposes one supported Playwright entrypoint:

- `GET /` over WebSocket

The only supported query parameter is:

- `browser=chromium|firefox|webkit`

If `browser` is omitted, the proxy uses `DEFAULT_BROWSER_TYPE`.

The proxy does not invent a custom browser protocol. A successful `connect()` returns a normal Playwright `Browser` connection to a worker that exposes a Playwright `launchServer` endpoint. Context creation still happens on the client side with `browser.newContext()` or the `browser.newPage()` convenience path.

Operational endpoints such as `/metrics` are not part of the Playwright connect contract.

## Browser Validation Rules

Browser types are validated in Go and treated as part of the public contract. Supported values are:

- `chromium`
- `firefox`
- `webkit`

The browser contract is intentionally centralized in Go so request validation and config validation use the same allowlist and error message.

The Redis selector still matches workers by browser type, but it does not maintain its own separate browser allowlist anymore. That avoids drift between the public contract and the worker-selection layer.

## Timeout Model

The proxy uses three separate timeout phases.

### 1. `PROXY_READ_HEADER_TIMEOUT`

This timeout covers only HTTP request header read.

Reason:

- slow or broken clients should not tie up sockets forever
- this is an HTTP server concern, not a worker-selection concern

### 2. `PROXY_WORKER_SELECTION_TIMEOUT`

This timeout covers the retry loop that waits for an eligible worker.

Reason:

- this is the main user-facing queue timeout
- if all matching workers are busy, the proxy should wait for a worker instead of failing immediately
- clients care most about how long they wait for capacity

This timeout is the answer to the question: "How long will the proxy wait for a worker to become available?"

### 3. `PROXY_CONNECT_TIMEOUT`

This timeout starts only after a worker is selected. It covers:

- backend worker dial
- client WebSocket upgrade handoff

Reason:

- waiting for capacity and handing off to a selected worker are different phases
- separating them keeps the error contract honest
- it also avoids awkward coupling between long queue waits and post-selection handoff failures

## Why the Timeout Model Is Split

Earlier versions tried to treat the whole pre-upgrade path as one timeout. That looked simple, but it created two problems:

- it mixed together three different operational questions: slow clients, no available worker yet, and failure to finish the handoff after a worker was chosen
- it broke on keep-alive reuse when timeout accounting was tied to the TCP connection instead of the individual request

The current model is request-scoped and phase-scoped on purpose. The worker-wait timeout is explicit because that is the setting users are most likely to care about and tune.

## Error Semantics

The proxy keeps a small client-facing error contract.

### Stable JSON Errors Before Client Upgrade

Before the client WebSocket upgrade begins, proxy-owned failures are returned as structured JSON:

```json
{"error":{"code":503,"message":"worker selection timed out"}}
```

Stable client-facing messages in this phase include:

- `unsupported browser; allowed values: chromium, firefox, webkit`
- `unsupported query parameters; only browser is allowed`
- `websocket upgrade required`
- `worker selection timed out`
- `connect timed out after selecting worker`
- `selected worker unavailable`

### Meaning of the Timeout Messages

`worker selection timed out` means:

- the proxy kept retrying for an eligible worker
- no worker became available before `PROXY_WORKER_SELECTION_TIMEOUT` expired

This is deliberate. The proxy does not currently expose a fail-fast public error like `no available workers`. In the current retrying model, the real terminal client-visible failure is timeout, not an instantaneous "none available right now" signal.

`connect timed out after selecting worker` means:

- the proxy already selected a worker
- the remaining handoff did not finish before `PROXY_CONNECT_TIMEOUT` expired

This covers post-selection timeout conditions such as:

- backend dial timeout
- connect budget already exhausted before client upgrade starts

`selected worker unavailable` means:

- the proxy selected a worker
- the failure was not classified as a timeout
- the selected worker or its endpoint appears invalid, unreachable, or otherwise failed during the backend connection phase

### Errors During `Upgrade()`

Once Gorilla's `websocket.Upgrader.Upgrade()` begins, the proxy no longer guarantees a fresh JSON HTTP error body.

Reason:

- Gorilla may hijack the socket before returning an error
- after hijack, the proxy cannot reliably write a new JSON HTTP response through `http.ResponseWriter`

In this phase, the proxy still:

- classifies the failure internally
- logs the detailed cause
- rolls back Redis bookkeeping
- closes the connection if needed

But it does not promise a structured client-visible JSON response.

## Handshake Validation Boundary

The proxy performs a small amount of manual WebSocket preflight validation before selecting a worker. That validation is intentionally limited to:

- HTTP method
- `Sec-WebSocket-Version`
- `Sec-WebSocket-Key`

Reason:

- malformed handshakes should be rejected before a worker is selected or a backend connection is opened
- Gorilla does not expose a public dry-run validation API for this

This is a deliberate tradeoff. It duplicates a small part of Gorilla's behavior, but it keeps malformed client requests from pinning workers unnecessarily. The preflight should stay narrow and well-tested.

## Cleanup and Bookkeeping Rules

Worker selection increments Redis counters during selection. That means the proxy must roll those counters back if setup fails later.

Cleanup work uses a detached bookkeeping context instead of the request context.

Reason:

- long-lived sessions outlive request-scoped connect deadlines
- request cancellation should not prevent counter rollback or final active-connection decrement

This is not optional bookkeeping. Without detached cleanup, a healthy worker can be left looking full even after the client is gone.

## Non-Goals

The current contract intentionally does not do the following:

- expose every transport-level failure detail to clients
- guarantee JSON after Gorilla has started the client upgrade
- provide a public fail-fast `no available workers` error while selection is retry-based
- blackhole or blacklist workers automatically in the proxy when a selected worker fails
- retry different workers after a selected worker has already been chosen

Those behaviors would expand the public contract or change proxy semantics. They should be introduced only as explicit follow-up decisions.

## Maintenance Guidance

If you change the connect path, keep these invariants intact:

- worker wait and post-selection handoff remain separate timeout phases
- client-visible timeout messages describe terminal outcomes, not transient internal states
- JSON is only promised before client upgrade begins
- cleanup must stay detached from request cancellation
- browser validation stays centralized in Go

If a future change breaks one of those points, update this document, the README, [TIMEOUTS.md](/Users/mbroton/projects/playwright-distributed/docs/TIMEOUTS.md), and the handler tests together.
