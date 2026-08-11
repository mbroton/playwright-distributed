# Maintenance audit

Date: 2026-08-11

Baseline: `af1d1d7` (`main` after pull request #69)

## Decision

Keep the current Go proxy, TypeScript worker, and Redis boundary. It is small,
understandable, and supports multiple proxy and worker processes. A rewrite
would delay the maintenance release without fixing the immediate risks.

The release should focus on safe upgrades, failure handling, and an honest
deployment contract. Optimize the selector only after measuring it at a stated
worker and connection target.

## Verified baseline

| Check | Result |
| --- | --- |
| `go test -race -cover ./...` | Pass. Proxy handler coverage is 84.9%; Redis, config, HTTP helpers, and entrypoint are untested. |
| `go vet ./...` | Pass. |
| Worker TypeScript check | Pass locally with `tsc --noEmit`; CI does not run it. |
| `npm audit` | No known vulnerabilities in the locked worker tree. |
| `govulncheck ./...` | Fail. It reports reachable `mapstructure/v2` vulnerabilities and a vulnerability in the local Go 1.26.4 toolchain. |
| Playwright package/image version check | Pass at 1.58.1. |
| Both Compose files | Valid. |
| Image builds and Chromium smoke test | Pass with a matching 1.58 client. Firefox and WebKit were not exercised. |
| GitHub Actions on `main` | Pass. CI checks proxy tests, version equality, and image builds. |

The repository's smoke client is not a reliable release check: it uses
Playwright 1.53 while the server uses 1.58. Playwright requires the connecting
client's major and minor version to match the server.

## Release blockers

Work through these in order, using one branch and pull request for each item.

### 1. Make coupled Playwright updates atomic

Dependabot updates `worker/package.json` and `worker/Dockerfile` in separate
pull requests. The version invariant correctly rejects both halves: pull
requests #62 and #70 demonstrate the failure.

Use a Dependabot multi-ecosystem group containing only `playwright-core` and
the Playwright Docker image. Keep the version check. Put the Conventional
Commit configuration on the group, because `commit-message` is group-only for
multi-ecosystem updates. Close the superseded single-ecosystem pull requests
after the grouped update is verified.

### 2. Restore a supported dependency and toolchain baseline

CI pins Go 1.23.1 and the proxy image tracks Go 1.23. Go supports only the two
newest major releases; 1.23 is unsupported. `govulncheck` also reaches
`mapstructure/v2` 2.2.1 issues fixed in 2.4.0.

Upgrade the Go toolchain and proxy modules, then run tests, the race detector,
`go vet`, `govulncheck`, and the proxy image build. Upgrade worker dependencies
in separate risk groups. In particular, test the Redis 4 to 6 major upgrade on
its own and keep Node types aligned with the Node runtime instead of blindly
selecting the newest major.

### 3. Fix worker Redis failure handling and secret logging

Both Node Redis clients lack an `error` listener. Node Redis documents this as
mandatory; a network error without one is thrown and exits the process. The
worker also logs its full configuration and Redis URL, which exposes credentials
when the URL contains them.

Register listeners before connecting, redact connection details, and cover
startup failure, reconnect, and shutdown behavior before the Redis major
upgrade.

### 4. Define and enforce the security boundary

The proxy has no authentication or TLS and accepts every WebSocket origin. A
connected client controls a browser that can reach the worker's network. The
worker runs the official Playwright image as root, which disables the Chromium
sandbox. This conflicts with the README's scraping and AI-agent use cases.

Before release, make the supported threat model explicit and make the default
deployment private or authenticated. For untrusted sites, run browsers as a
separate user with the recommended sandbox/seccomp setup. Do not describe the
current root container as safe for arbitrary public crawling.

### 5. Add the checks needed for safe upgrades

Add worker type-checking to CI and replace the stale smoke client with one tied
to the server version. Add a small Redis-backed integration test for selection,
counter rollback, draining, and stale-worker cleanup. Keep the existing image
builds. Full unit coverage is not required for this release.

### 6. Correct the operating documentation

`TIMEOUTS.md` disagrees with the code about heartbeat, key TTL, drain timeout,
and reaping. The selector has a hard-coded 60-second heartbeat threshold, while
worker timing is configurable without cross-field validation. The README also
omits the Playwright client/server version constraint.

Make code and docs agree, validate timing relationships at startup, and document
the exact release smoke command.

### 7. Make the tag-to-release path fail safely

The release workflow starts publishing images for any `v*` tag and pushes the
proxy before it knows the worker will succeed. It does not verify that the tag
points to tested `main`.

Require a successful validation job before publishing, verify the tagged commit
belongs to `main`, and build both images before either push step. Keep the manual
release checklist as the final guard, including pulls and smoke tests of the
published tags.

## Near-term follow-ups

These are real issues, but they should not expand the maintenance release.

1. **Make session accounting recoverable.** The selector increments counters
   before a second Redis read. If the worker key expires between those steps,
   the reservation leaks. Abrupt proxy exits also leave active counts on live
   workers. Introduce an explicit reservation/session lease or another design
   that can be reconciled after proxy failure.
2. **Finish proxy lifecycle handling.** Add Redis startup retries, health and
   readiness endpoints, signal handling, graceful HTTP shutdown, and bounded
   contexts for cleanup. Compose `depends_on` currently orders startup but does
   not wait for Redis readiness.
3. **Exercise every browser.** Run one exact-version Chromium, Firefox, and
   WebKit connection in the release path.
4. **Clarify metrics.** `/metrics` returns a process-local JSON count, not
   cluster metrics or Prometheus output. Rename it or expose useful cluster and
   lifecycle metrics.

## Optional, measure first

The Lua selector scans both counter hashes and reads metadata for every worker
while Redis is blocked in the script. That is O(workers) per connection and can
become the scaling ceiling. First add a benchmark with a realistic target. Move
to indexed candidates or another selection model only if the target fails.

Defer Camoufox support (#28). Revisit automatic fallback (#29) after session
reservations are recoverable; fallback before that would add more ambiguous
counter states.

## Sources

- [GitHub: multi-ecosystem Dependabot updates](https://docs.github.com/en/code-security/concepts/supply-chain-security/multi-ecosystem-updates)
- [Playwright: client/server version compatibility](https://playwright.dev/docs/api/class-browsertype#browser-type-connect)
- [Playwright: Docker security guidance](https://playwright.dev/docs/docker)
- [Node Redis: required error event handling](https://github.com/redis/node-redis#events)
- [Go release policy](https://go.dev/doc/devel/release#policy)
- [Go vulnerability GO-2025-3900](https://pkg.go.dev/vuln/GO-2025-3900)
- [Docker Compose startup readiness](https://docs.docker.com/compose/how-tos/startup-order/)
