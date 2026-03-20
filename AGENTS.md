# Repository Guidelines

## Project Structure & Module Organization
`proxy/` contains the Go WebSocket proxy. Keep the executable entrypoint in `proxy/cmd/proxy/main.go`, proxy and Redis logic under `proxy/internal/`, and reusable config/logging helpers in `proxy/pkg/`.

`worker/` contains the TypeScript Playwright worker. Runtime code lives in `worker/src/`, local env defaults in `worker/.env.example`, and the runtime image definition in `worker/Dockerfile`.

`scripts/` contains development helpers such as `scripts/check-playwright-version.js`, `scripts/test/`, and `scripts/monitor/`.

## Build, Test, and Development Commands
`docker compose up -d` starts the published stack locally: Redis, proxy, and one Chromium worker.

`docker compose -f docker-compose.local.yaml up --build` builds local images and starts Chromium, Firefox, and WebKit workers for development.

`cd proxy && go test -v ./...` runs the proxy unit test suite used in CI.

`node scripts/check-playwright-version.js` verifies that `worker/package.json` and `worker/Dockerfile` use the same Playwright version.

`cd worker && npm run start` runs a worker directly once `REDIS_URL`, `PORT`, and related env vars are set.

## Coding Style & Naming Conventions
Follow existing language conventions rather than introducing repo-wide tooling. Go code should stay `gofmt`-clean, use lowercase package names, and keep `internal/` boundaries intact. TypeScript uses ES modules, explicit `.js` import suffixes, strict typing, and 4-space indentation. Use `PascalCase` for types/classes, `camelCase` for functions and variables, and uppercase names for env vars.

Keep Playwright versions synchronized across `worker/package.json` and `worker/Dockerfile`; CI enforces this.

## Testing Guidelines
Add Go tests beside the code they cover as `*_test.go`. Prefer table-driven tests for handler and selection logic. CI currently checks proxy tests, Docker image builds, and Playwright version sync; there is no dedicated worker unit suite yet, so worker changes should include a manual smoke test with `docker-compose.local.yaml`.

## Commit & Pull Request Guidelines
Recent history favors short, imperative commit subjects. Dependency bumps follow `Bump <package> from <old> to <new> in /worker`; keep that format for version-only updates.

Keep PRs narrowly scoped to one area when possible. Include a brief behavior summary, note any env or Docker changes, link related issues, and list the commands you ran to verify the change.
