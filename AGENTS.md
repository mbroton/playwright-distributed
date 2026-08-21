# Project instructions

This repository contains a Go control-plane server (WebSocket relay + REST
API) and a TypeScript Playwright worker.

## Constraints

- Preserve the server/worker boundary unless the task explicitly changes the
  architecture.
- Keep `worker/package.json`'s `playwright-core` version equal to the Playwright
  image version in `worker/Dockerfile`. Update them together.
- Worker code uses ES modules. Keep explicit `.js` suffixes in local imports.
- Avoid unrelated refactors, dependencies, or repository-wide tooling changes.

## Verification

- Server changes: run `cd server && go test ./...` (needs a running Docker
  daemon for testcontainers).
- Worker code changes: run `cd worker && npm test && npm run typecheck`.
- Playwright version changes: run `node scripts/check-playwright-version.js`.
- Worker runtime changes: smoke-test with
  `docker compose -f docker-compose.local.yaml up --build`.
- Run the checks relevant to the change and report anything you could not run.

## Git workflow

- Use one branch and one pull request per task.
- Use Conventional Commits, for example `fix(server): handle closed connections`
  or `chore(deps): update Playwright`.
- Never add AI attribution or co-author trailers.
- Keep pull requests focused. Include the behavior change, configuration impact,
  and verification performed.
