import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import http, { type IncomingMessage, type ServerResponse } from 'node:http';
import test from 'node:test';
import WebSocket, { WebSocketServer } from 'ws';
import { ControlPlaneClient, ControlPlaneError } from './control-plane.js';
import type { WorkerConfig } from './config.js';
import { SessionGateway } from './gateway.js';
import type {
    BrowserServerLike,
    ControlPlaneLike,
    GatewayLike,
    WorkerRegistration,
} from './worker.js';
import { BrowserWorker } from './worker.js';

test('control-plane client sends bearer authorization on every API call', async t => {
    const authorizationHeaders: Array<string | undefined> = [];
    const server = await startControlPlane(async (request, response) => {
        authorizationHeaders.push(request.headers.authorization);
        if (request.url === '/internal/workers') {
            sendJSON(response, 201, { id: 'worker-1' });
            return;
        }
        if (request.url?.endsWith('/heartbeat')) {
            sendJSON(response, 200, { status: 'available', stale_session_ids: [] });
            return;
        }
        sendJSON(response, 200, { id: 'worker-1' });
    });
    t.after(() => server.close());

    const controlPlane = new ControlPlaneClient(server.url, 'worker-secret');
    await controlPlane.register(registration, 1);
    await controlPlane.heartbeat('worker-1', []);
    await controlPlane.recycle('worker-1');
    await controlPlane.setStatus('worker-1', 'draining');

    assert.deepEqual(authorizationHeaders, [
        'Bearer worker-secret',
        'Bearer worker-secret',
        'Bearer worker-secret',
        'Bearer worker-secret',
    ]);
});

test('control-plane clients keep independent base URLs and authorization', async t => {
    const firstRequests: Array<string | undefined> = [];
    const secondRequests: Array<string | undefined> = [];
    const first = await startControlPlane(async (request, response) => {
        firstRequests.push(request.headers.authorization);
        sendJSON(response, 201, { id: 'worker-1' });
    });
    const second = await startControlPlane(async (request, response) => {
        secondRequests.push(request.headers.authorization);
        sendJSON(response, 201, { id: 'worker-2' });
    });
    t.after(() => Promise.all([first.close(), second.close()]));

    const firstClient = new ControlPlaneClient(first.url, 'first-secret');
    const secondClient = new ControlPlaneClient(second.url, 'second-secret');
    assert.equal((await firstClient.register(registration, 1)).id, 'worker-1');
    assert.equal((await secondClient.register(registration, 1)).id, 'worker-2');

    assert.deepEqual(firstRequests, ['Bearer first-secret']);
    assert.deepEqual(secondRequests, ['Bearer second-secret']);
});

test('registration stops after one non-retryable response', async t => {
    let attempts = 0;
    const server = await startControlPlane(async (_request, response) => {
        attempts += 1;
        sendJSON(response, 422, { detail: 'address is invalid' });
    });
    t.after(() => server.close());

    const controlPlane = new ControlPlaneClient(server.url);
    await assert.rejects(
        controlPlane.register(registration),
        /HTTP 422: address is invalid/,
    );
    assert.equal(attempts, 1);
});

test('a hung registration request is aborted by its per-attempt timeout', async t => {
    let requestAborted = false;
    const server = await startControlPlane(async request => {
        request.on('aborted', () => {
            requestAborted = true;
        });
    });
    t.after(() => server.close());

    const controlPlane = new ControlPlaneClient(server.url, undefined, 5_000, undefined, 20);
    await assert.rejects(
        controlPlane.register(registration, 1),
        /register worker failed after 1 attempts/,
    );
    await waitFor(() => requestAborted);
});

test('registration reuses its instance ID for retries and renews it for a new call', async t => {
    const bodies: Array<WorkerRegistration & { instance_id: string }> = [];
    const server = await startControlPlane(async (request, response) => {
        bodies.push(await readJSON(request));
        if (bodies.length === 1) {
            sendJSON(response, 500, { detail: 'response lost' });
            return;
        }
        sendJSON(response, 201, { id: `worker-${bodies.length}` });
    });
    t.after(() => server.close());

    const controlPlane = new ControlPlaneClient(server.url);
    await controlPlane.register(registration, 2, 0);
    await controlPlane.register(registration, 1);

    assert.equal(bodies.length, 3);
    assert.equal(bodies[0]?.instance_id, bodies[1]?.instance_id);
    assert.notEqual(bodies[1]?.instance_id, bodies[2]?.instance_id);
    assert.match(bodies[0]?.instance_id ?? '', /^[0-9a-f-]{36}$/);
});

test('recycle stops after one 404 response', async t => {
    let attempts = 0;
    const server = await startControlPlane(async (_request, response) => {
        attempts += 1;
        sendJSON(response, 404, { detail: 'worker not found' });
    });
    t.after(() => server.close());

    const controlPlane = new ControlPlaneClient(server.url);
    await assert.rejects(
        controlPlane.recycle('worker-1'),
        /HTTP 404: worker not found/,
    );
    assert.equal(attempts, 1);
});

test('heartbeat is single-flight, reports sessions, and closes stale sessions', async t => {
    let concurrentHeartbeats = 0;
    let maxConcurrentHeartbeats = 0;
    const heartbeatBodies: Array<{ active_session_ids: string[] }> = [];
    const server = await startControlPlane(async (request, response) => {
        if (request.url === '/internal/workers' && request.method === 'POST') {
            sendJSON(response, 201, { id: 'worker-1' });
            return;
        }
        if (request.url === '/internal/workers/worker-1/heartbeat') {
            concurrentHeartbeats += 1;
            maxConcurrentHeartbeats = Math.max(maxConcurrentHeartbeats, concurrentHeartbeats);
            heartbeatBodies.push(await readJSON(request));
            await delay(4);
            concurrentHeartbeats -= 1;
            sendJSON(response, 200, {
                status: 'available',
                stale_session_ids: heartbeatBodies.length === 1 ? [activeId] : [],
            });
            return;
        }
        sendJSON(response, 200, { id: 'worker-1' });
    });
    t.after(() => server.close());

    const gateway = new FakeGateway([activeId]);
    const worker = createWorker(server.url, gateway);
    await worker.start();
    await waitFor(() => gateway.closedSessions.length === 1);
    await delay(80);
    await worker.shutdown(0);

    assert.equal(maxConcurrentHeartbeats, 1);
    assert.deepEqual(heartbeatBodies[0], { active_session_ids: [activeId] });
    assert.deepEqual(gateway.closedSessions, [{ sessionId: activeId, code: 1001 }]);
});

test('heartbeat requests time out and the loop keeps ticking', async t => {
    let heartbeatAttempts = 0;
    const server = await startControlPlane(async (request, response) => {
        if (request.url === '/internal/workers') {
            sendJSON(response, 201, { id: 'worker-1' });
            return;
        }
        if (request.url?.endsWith('/heartbeat')) {
            heartbeatAttempts += 1;
            return;
        }
        sendJSON(response, 200, { id: 'worker-1' });
    });
    t.after(() => server.close());

    const worker = createWorker(server.url, new FakeGateway());
    await worker.start();
    await waitFor(() => heartbeatAttempts >= 3, 250);
    await worker.shutdown(0);

    assert.ok(heartbeatAttempts >= 3);
});

test('heartbeat draining status enters drain mode', async t => {
    const server = await startControlPlane(async (request, response) => {
        if (request.url === '/internal/workers') {
            sendJSON(response, 201, { id: 'worker-1' });
            return;
        }
        if (request.url?.endsWith('/heartbeat')) {
            sendJSON(response, 200, { status: 'draining', stale_session_ids: [] });
            return;
        }
        sendJSON(response, 200, { id: 'worker-1' });
    });
    t.after(() => server.close());

    const worker = createWorker(server.url, new FakeGateway([activeId]));
    await worker.start();
    await waitFor(() => worker.state === 'draining');
    await worker.shutdown(0);

    assert.equal(worker.state, 'shutting_down');
});

test('a draining worker re-asserts drain intent after an available heartbeat', async () => {
    const controlPlane = new FakeControlPlane();
    const gateway = new FakeGateway([activeId]);
    const worker = createInjectedWorker(controlPlane, gateway, 5_000);
    await worker.start();
    await worker.requestDrain('SIGTERM');

    await waitFor(() => controlPlane.statuses.includes('draining'));
    await worker.shutdown(0);

    assert.equal(controlPlane.statuses[0], 'draining');
});

test('heartbeat 404 registers a new worker ID', async t => {
    let registrations = 0;
    const server = await startControlPlane(async (request, response) => {
        if (request.url === '/internal/workers') {
            registrations += 1;
            sendJSON(response, 201, { id: `worker-${registrations}` });
            return;
        }
        if (request.url === '/internal/workers/worker-1/heartbeat') {
            sendJSON(response, 404, { detail: 'worker not found' });
            return;
        }
        if (request.url === '/internal/workers/worker-2/heartbeat') {
            sendJSON(response, 200, { status: 'available', stale_session_ids: [] });
            return;
        }
        sendJSON(response, 200, { id: 'worker-2' });
    });
    t.after(() => server.close());

    const worker = createWorker(server.url, new FakeGateway());
    await worker.start();
    await waitFor(() => worker.workerId === 'worker-2');
    await worker.shutdown(0);

    assert.equal(registrations, 2);
});

test('heartbeat 404 re-asserts drain status immediately after registration', async () => {
    const events: string[] = [];
    let registrations = 0;
    let heartbeats = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => {
            registrations += 1;
            const workerId = `worker-${registrations}`;
            events.push(`register:${workerId}`);
            return { id: workerId };
        },
        recycle: async workerId => ({ id: workerId }),
        heartbeat: async workerId => {
            heartbeats += 1;
            events.push(`heartbeat:${workerId}`);
            if (heartbeats === 1) {
                throw new ControlPlaneError('worker not found', 404);
            }
            return { status: 'available', stale_session_ids: [] };
        },
        setStatus: async (workerId, status) => {
            events.push(`status:${workerId}:${status}`);
        },
    };
    const worker = createInjectedWorker(controlPlane, new FakeGateway([activeId]), 5_000);
    await worker.start();
    await worker.requestDrain('SIGTERM');

    await waitFor(() => events.includes('status:worker-2:draining'));
    assert.deepEqual(events.slice(0, 4), [
        'register:worker-1',
        'heartbeat:worker-1',
        'register:worker-2',
        'status:worker-2:draining',
    ]);
    await worker.shutdown(0);
});

test('drain with no connections shuts down promptly', async () => {
    const controlPlane = new FakeControlPlane();
    const gateway = new FakeGateway();
    const worker = createInjectedWorker(controlPlane, gateway, 5_000);
    await worker.start();

    const startedAt = Date.now();
    await worker.requestDrain('SIGTERM', true);
    assert.equal(await worker.waitForExit(), 0);

    assert.ok(Date.now() - startedAt < 200);
    assert.deepEqual(controlPlane.statuses, ['draining', 'shutting_down']);
    assert.equal(gateway.shutdownCalled, true);
});

test('a control-plane drain recycles the browser in place instead of exiting', async () => {
    const controlPlane = new FakeControlPlane();
    const gateway = new FakeGateway();
    const worker = createInjectedWorker(controlPlane, gateway, 5_000);
    await worker.start();

    let exited = false;
    void worker.waitForExit().then(() => {
        exited = true;
    });
    await worker.requestDrain('control plane');
    await waitFor(() => controlPlane.recycles.length === 1);

    assert.equal(worker.state, 'running');
    assert.equal(worker.workerId, 'worker-1');
    assert.equal(exited, false);
    assert.equal(gateway.shutdownCalled, false);
    assert.equal(controlPlane.registrations.length, 1);
    assert.deepEqual(controlPlane.recycles, ['worker-1']);
    assert.deepEqual(controlPlane.statuses, []);
    assert.equal(gateway.browserEndpoints.length, 1);
    await worker.shutdown(0);
});

test('drain timeout forces the browser swap with active connections', async () => {
    const controlPlane = new FakeControlPlane();
    const gateway = new FakeGateway([activeId]);
    const worker = createInjectedWorker(controlPlane, gateway, 30);
    await worker.start();

    await worker.requestDrain('control plane');
    await waitFor(() => controlPlane.recycles.length === 1);

    assert.equal(worker.state, 'running');
    assert.equal(gateway.activeConnectionCount, 0);
    assert.deepEqual(gateway.closedSessions, [{ sessionId: activeId, code: 1001 }]);
    assert.deepEqual(controlPlane.recycles, ['worker-1']);
    await worker.shutdown(0);
});

test('the worker keeps its ID and refreshes its session budget after recycle', async () => {
    const controlPlane = new FakeControlPlane(2, 3);
    const gateway = new FakeGateway([activeId, secondId]);
    const worker = createInjectedWorker(controlPlane, gateway, 5_000);
    await worker.start();

    gateway.closeSession(activeId);
    assert.equal(worker.state, 'running');

    gateway.closeSession(secondId);
    await waitFor(() => controlPlane.recycles.length === 1);
    assert.equal(worker.state, 'running');
    assert.equal(worker.workerId, 'worker-1');
    assert.equal(controlPlane.registrations.length, 1);

    gateway.addSession(thirdId);
    gateway.addSession(fourthId);
    gateway.addSession(fifthId);
    gateway.closeSession(thirdId);
    gateway.closeSession(fourthId);
    assert.equal(controlPlane.recycles.length, 1);
    gateway.closeSession(fifthId);
    await waitFor(() => controlPlane.recycles.length === 2);
    await worker.shutdown(0);
});

test('a heartbeat response during the swap cannot drain or stale-kill the recycled worker', async () => {
    let heartbeats = 0;
    let releaseFirstHeartbeat!: () => void;
    const firstHeartbeatGate = new Promise<void>(resolve => {
        releaseFirstHeartbeat = resolve;
    });
    const gateway = new FakeGateway();
    let recycles = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => ({ id: 'worker-1', max_lifetime_sessions: 2 }),
        recycle: async workerId => {
            recycles += 1;
            gateway.addSession(thirdId);
            releaseFirstHeartbeat();
            await delay(30);
            return { id: workerId, max_lifetime_sessions: 7 };
        },
        heartbeat: async () => {
            heartbeats += 1;
            if (heartbeats === 1) {
                await firstHeartbeatGate;
                return { status: 'draining' as const, stale_session_ids: [thirdId] };
            }
            return { status: 'available' as const, stale_session_ids: [] };
        },
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, gateway, 5_000);
    await worker.start();
    await waitFor(() => heartbeats >= 1);

    await worker.requestDrain('control plane');
    await delay(50);

    assert.equal(worker.state, 'running');
    assert.equal(worker.workerId, 'worker-1');
    assert.equal(recycles, 1);
    assert.deepEqual(gateway.activeSessionIds, [thirdId]);
    assert.deepEqual(gateway.closedSessions, []);
    await worker.shutdown(0);
});

test('a heartbeat response after the swap cannot drain or close a new session', async () => {
    let heartbeats = 0;
    let releaseFirstHeartbeat!: () => void;
    const firstHeartbeatGate = new Promise<void>(resolve => {
        releaseFirstHeartbeat = resolve;
    });
    const gateway = new FakeGateway();
    let recycles = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => ({ id: 'worker-1' }),
        recycle: async workerId => {
            recycles += 1;
            return { id: workerId };
        },
        heartbeat: async () => {
            heartbeats += 1;
            if (heartbeats === 1) {
                await firstHeartbeatGate;
                return { status: 'draining' as const, stale_session_ids: [] };
            }
            return { status: 'available' as const, stale_session_ids: [] };
        },
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, gateway, 30);
    await worker.start();
    await waitFor(() => heartbeats >= 1);

    await worker.requestDrain('control plane');
    assert.equal(worker.state, 'running');
    assert.equal(recycles, 1);
    gateway.addSession(thirdId);
    releaseFirstHeartbeat();
    await delay(80);

    assert.equal(worker.state, 'running');
    assert.equal(recycles, 1);
    assert.deepEqual(gateway.activeSessionIds, [thirdId]);
    assert.deepEqual(gateway.closedSessions, []);
    await worker.shutdown(0);
});

test('a heartbeat dispatched during the swap is discarded after the recycle completes', async () => {
    let releaseRecycle!: () => void;
    const recycleGate = new Promise<void>(resolve => {
        releaseRecycle = resolve;
    });
    let releaseSwapHeartbeat!: () => void;
    const swapHeartbeatGate = new Promise<void>(resolve => {
        releaseSwapHeartbeat = resolve;
    });
    let heartbeats = 0;
    let recycles = 0;
    let recycling = false;
    let swapHeartbeats = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => ({ id: 'worker-1' }),
        recycle: async workerId => {
            recycles += 1;
            recycling = true;
            await recycleGate;
            recycling = false;
            return { id: workerId };
        },
        heartbeat: async () => {
            heartbeats += 1;
            if (recycling && swapHeartbeats === 0) {
                swapHeartbeats += 1;
                await swapHeartbeatGate;
                // The server row stays draining for the whole swap.
                return { status: 'draining' as const, stale_session_ids: [] };
            }
            return { status: 'available' as const, stale_session_ids: [] };
        },
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, new FakeGateway(), 20);
    await worker.start();
    await waitFor(() => heartbeats >= 1);

    const drainPromise = worker.requestDrain('control plane');
    await waitFor(() => swapHeartbeats === 1);
    releaseRecycle();
    await drainPromise;
    assert.equal(worker.state, 'running');
    assert.equal(recycles, 1);

    releaseSwapHeartbeat();
    await delay(60);

    assert.equal(worker.state, 'running');
    assert.equal(recycles, 1);
    await worker.shutdown(0);
});

test('a recycle succeeds after transient 503 responses without exiting', async t => {
    let registrations = 0;
    let recycleAttempts = 0;
    const server = await startControlPlane(async (request, response) => {
        if (request.url === '/internal/workers') {
            registrations += 1;
            sendJSON(response, 201, { id: 'worker-1' });
            return;
        }
        if (request.url?.endsWith('/recycle')) {
            recycleAttempts += 1;
            if (recycleAttempts < 3) {
                sendJSON(response, 503, { detail: 'control plane restarting' });
                return;
            }
            sendJSON(response, 200, { id: 'worker-1' });
            return;
        }
        if (request.url?.endsWith('/heartbeat')) {
            sendJSON(response, 200, { status: 'available', stale_session_ids: [] });
            return;
        }
        sendJSON(response, 200, { id: 'worker-1' });
    });
    t.after(() => server.close());

    const exitCodes: number[] = [];
    const worker = new BrowserWorker(testConfig(5_000), {
        controlPlane: new ControlPlaneClient(
            server.url,
            undefined,
            5_000,
            async () => undefined,
            100,
        ),
        createGateway: () => new FakeGateway(),
        launchBrowser: async () => new FakeBrowser(),
        exit: code => exitCodes.push(code),
    });
    await worker.start();

    await worker.requestDrain('control plane');

    assert.equal(worker.state, 'running');
    assert.equal(registrations, 1);
    assert.equal(recycleAttempts, 3);
    assert.deepEqual(exitCodes, []);
    await worker.shutdown(0);
});

test('a recycle 404 falls back to a new worker registration', async () => {
    let registrations = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => {
            registrations += 1;
            return { id: `worker-${registrations}`, max_lifetime_sessions: 0 };
        },
        recycle: async () => {
            throw new ControlPlaneError('worker not found', 404);
        },
        heartbeat: async () => ({ status: 'available', stale_session_ids: [] }),
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, new FakeGateway(), 5_000);
    await worker.start();

    await worker.requestDrain('control plane');

    assert.equal(registrations, 2);
    assert.equal(worker.workerId, 'worker-2');
    await worker.shutdown(0);
});

test('a recycle 409 exits cleanly without registering again', async () => {
    let registrations = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => {
            registrations += 1;
            return { id: 'worker-1' };
        },
        recycle: async () => {
            throw new ControlPlaneError('worker is shutting down', 409);
        },
        heartbeat: async () => ({ status: 'available', stale_session_ids: [] }),
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, new FakeGateway(), 5_000);
    await worker.start();

    await worker.requestDrain('control plane');

    assert.equal(await worker.waitForExit(), 0);
    assert.equal(registrations, 1);
    assert.equal(worker.state, 'shutting_down');
});

test('a heartbeat 404 resolving during recycle does not register a duplicate worker', async () => {
    let releaseHeartbeat!: () => void;
    const heartbeatGate = new Promise<void>(resolve => {
        releaseHeartbeat = resolve;
    });
    let heartbeats = 0;
    let registrations = 0;
    let recycles = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => {
            registrations += 1;
            return { id: `worker-${registrations}`, max_lifetime_sessions: 0 };
        },
        recycle: async workerId => {
            recycles += 1;
            releaseHeartbeat();
            await delay(30);
            return { id: workerId, max_lifetime_sessions: 0 };
        },
        heartbeat: async () => {
            heartbeats += 1;
            if (heartbeats === 1) {
                await heartbeatGate;
                throw new ControlPlaneError('worker not found', 404);
            }
            return { status: 'available' as const, stale_session_ids: [] };
        },
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, new FakeGateway(), 5_000);
    await worker.start();
    await waitFor(() => heartbeats >= 1);

    await worker.requestDrain('control plane');
    await delay(50);

    assert.equal(worker.state, 'running');
    assert.equal(worker.workerId, 'worker-1');
    assert.equal(registrations, 1);
    assert.equal(recycles, 1);
    await worker.shutdown(0);
});

test('an in-flight 404 re-registration is shared with a recycle register fallback', async () => {
    let releaseRegister!: () => void;
    const registerGate = new Promise<void>(resolve => {
        releaseRegister = resolve;
    });
    let registrations = 0;
    let recycles = 0;
    let heartbeats = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => {
            registrations += 1;
            if (registrations === 2) {
                await registerGate;
            }
            return { id: `worker-${registrations}` };
        },
        recycle: async () => {
            recycles += 1;
            throw new ControlPlaneError('worker not found', 404);
        },
        heartbeat: async () => {
            heartbeats += 1;
            if (heartbeats === 1) {
                throw new ControlPlaneError('worker not found', 404);
            }
            return { status: 'available' as const, stale_session_ids: [] };
        },
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, new FakeGateway(), 20);
    await worker.start();
    // The heartbeat 404 starts a re-registration that is held in flight.
    await waitFor(() => registrations === 2);

    // A recycle 404 must join that registration, not start a third one.
    const drainPromise = worker.requestDrain('control plane');
    await delay(40);
    releaseRegister();
    await drainPromise;
    await delay(40);

    assert.equal(worker.state, 'running');
    assert.equal(worker.workerId, 'worker-2');
    assert.equal(registrations, 2);
    assert.equal(recycles, 1);
    await worker.shutdown(0);
});

test('the replacement endpoint is swapped before the old browser closes', async () => {
    const events: string[] = [];
    class LoggingGateway extends FakeGateway {
        override setBrowserEndpoint(endpoint: string): void {
            events.push(`swap:${endpoint}`);
            super.setBrowserEndpoint(endpoint);
        }
    }
    class LoggingBrowser extends FakeBrowser {
        constructor(private readonly name: string) {
            super();
        }

        override wsEndpoint(): string {
            return `ws://127.0.0.1:12345/${this.name}`;
        }

        override async close(): Promise<void> {
            events.push(`close:${this.name}`);
            await super.close();
        }
    }
    let launches = 0;
    const worker = new BrowserWorker(testConfig(5_000), {
        controlPlane: new FakeControlPlane(),
        createGateway: () => new LoggingGateway(),
        launchBrowser: async () => {
            launches += 1;
            return new LoggingBrowser(`browser-${launches}`);
        },
        exit: () => undefined,
    });
    await worker.start();

    await worker.requestDrain('control plane');
    assert.deepEqual(events, ['swap:ws://127.0.0.1:12345/browser-2', 'close:browser-1']);
    await worker.shutdown(0);
});

test('heartbeat-404 re-registration keeps the spent session budget', async () => {
    let registrations = 0;
    let recycles = 0;
    let heartbeats = 0;
    const controlPlane: ControlPlaneLike = {
        register: async () => {
            registrations += 1;
            return { id: `worker-${registrations}`, max_lifetime_sessions: 2 };
        },
        recycle: async workerId => {
            recycles += 1;
            return { id: workerId, max_lifetime_sessions: 2 };
        },
        heartbeat: async () => {
            heartbeats += 1;
            if (heartbeats === 1) {
                throw new ControlPlaneError('worker not found', 404);
            }
            return { status: 'available' as const, stale_session_ids: [] };
        },
        setStatus: async () => undefined,
    };
    const gateway = new FakeGateway([activeId, secondId]);
    const worker = createInjectedWorker(controlPlane, gateway, 5_000);
    await worker.start();

    gateway.closeSession(activeId); // budget 1 of 2 spent
    await waitFor(() => registrations === 2); // 404 re-registers, same browser

    gateway.closeSession(secondId); // 2 of 2 — must drain despite re-registration
    await waitFor(() => recycles === 1);
    assert.equal(registrations, 2);
    assert.equal(worker.state, 'running');
    await worker.shutdown(0);
});

test('a recycle that cannot stop the old browser exits for a supervisor restart', async () => {
    const controlPlane = new FakeControlPlane();
    const wedged = new WedgedBrowser();
    let launches = 0;
    const worker = new BrowserWorker(testConfig(5_000), {
        controlPlane,
        createGateway: () => new FakeGateway(),
        launchBrowser: async () => {
            launches += 1;
            return launches === 1 ? wedged : new FakeBrowser();
        },
        exit: () => undefined,
        browserCloseTimeoutMs: 20,
        cleanupTimeoutMs: 20,
    });
    await worker.start();

    await worker.requestDrain('control plane');
    assert.equal(await worker.waitForExit(), 1);
});

test('a failed browser relaunch exits for a supervisor restart', async () => {
    const controlPlane = new FakeControlPlane();
    let launches = 0;
    const worker = new BrowserWorker(testConfig(5_000), {
        controlPlane,
        createGateway: () => new FakeGateway(),
        launchBrowser: async () => {
            launches += 1;
            if (launches > 1) {
                throw new Error('no browsers left');
            }
            return new FakeBrowser();
        },
        exit: () => undefined,
    });
    await worker.start();

    await worker.requestDrain('control plane');
    assert.equal(await worker.waitForExit(), 1);
});

test('drain timeout swaps the browser under a live session on the real gateway', async t => {
    const browsers: NetworkBrowser[] = [];
    const launchNetworkBrowser = async (): Promise<NetworkBrowser> => {
        const server = new WebSocketServer({ host: '127.0.0.1', port: 0 });
        await waitForWebSocketServer(server);
        const address = server.address();
        if (!address || typeof address === 'string') {
            throw new Error('browser fixture did not use a TCP port');
        }
        server.on('connection', socket => socket.on('message', data => socket.send(data)));
        const browser = new NetworkBrowser(server, `ws://127.0.0.1:${address.port}`);
        browsers.push(browser);
        return browser;
    };
    const controlPlane = new FakeControlPlane();
    let gateway!: SessionGateway;
    const worker = new BrowserWorker(testConfig(30, 0), {
        controlPlane,
        createGateway: (endpoint, port) => {
            gateway = new SessionGateway(endpoint, port, '127.0.0.1');
            return gateway;
        },
        launchBrowser: launchNetworkBrowser,
        exit: () => undefined,
    });
    t.after(async () => {
        await worker.shutdown(0);
        for (const browser of browsers) {
            await browser.kill();
        }
    });
    await worker.start();
    const client = await connectWebSocket(`ws://${'127.0.0.1'}:${gateway.listeningPort}`, activeId);
    await waitFor(() => gateway?.activeConnectionCount === 1);

    await worker.requestDrain('control plane');
    await waitFor(() => controlPlane.recycles.length === 1);

    assert.equal(worker.state, 'running');
    assert.equal(browsers.length, 2);
    assert.equal(gateway.activeConnectionCount, 0);
    client.terminate();

    // A post-swap session must reach the replacement browser end to end.
    const postSwap = await connectWebSocket(`ws://127.0.0.1:${gateway.listeningPort}`, secondId);
    const echoed = new Promise<string>(resolve => postSwap.once('message', data => resolve(String(data))));
    postSwap.send('after-swap');
    assert.equal(await bounded(echoed, 1_000), 'after-swap');
    postSwap.terminate();
});

test('startup does not resume after shutdown wins a registration race', async () => {
    let resolveRegistration!: (worker: { id: string }) => void;
    let registrationStarted = false;
    let heartbeatCalls = 0;
    const controlPlane: ControlPlaneLike = {
        register: () => {
            registrationStarted = true;
            return new Promise(resolve => {
                resolveRegistration = resolve;
            });
        },
        recycle: async workerId => ({ id: workerId }),
        heartbeat: async () => {
            heartbeatCalls += 1;
            return { status: 'available', stale_session_ids: [] };
        },
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, new FakeGateway(), 5_000);
    const start = worker.start();
    await waitFor(() => registrationStarted);

    await worker.requestDrain('SIGTERM', true);
    resolveRegistration({ id: 'late-worker' });
    await start;
    await delay(40);

    assert.equal(worker.state, 'shutting_down');
    assert.equal(heartbeatCalls, 0);
});

test('a hanging cleanup step cannot block worker exit', async () => {
    const worker = new BrowserWorker(testConfig(5_000), {
        controlPlane: new FakeControlPlane(),
        createGateway: () => new HangingShutdownGateway(),
        launchBrowser: async () => new FakeBrowser(),
        exit: () => undefined,
        cleanupTimeoutMs: 20,
    });
    await worker.start();

    await worker.requestDrain('SIGTERM', true);
    assert.equal(await bounded(worker.waitForExit(), 200), 0);
});

test('registration uses the port that the gateway bound', async () => {
    const controlPlane = new FakeControlPlane();
    const worker = createInjectedWorker(controlPlane, new FakeGateway([], 4321), 5_000);
    await worker.start();
    await worker.shutdown(0);

    assert.equal(controlPlane.registrations[0]?.address, 'ws://worker-test:4321/');
});

test('browser close timeout kills the browser before exit', async () => {
    const controlPlane = new FakeControlPlane();
    const gateway = new FakeGateway();
    const browser = new FakeBrowser(true);
    const worker = new BrowserWorker(testConfig(5_000), {
        controlPlane,
        createGateway: () => gateway,
        launchBrowser: async () => browser,
        exit: () => undefined,
        browserCloseTimeoutMs: 20,
    });
    await worker.start();

    await worker.requestDrain('SIGTERM', true);
    assert.equal(await worker.waitForExit(), 0);
    assert.equal(browser.killCalled, true);
});

const activeId = '22222222-2222-4222-8222-222222222222';
const secondId = '33333333-3333-4333-8333-333333333333';
const thirdId = '44444444-4444-4444-8444-444444444444';
const fourthId = '55555555-5555-4555-8555-555555555555';
const fifthId = '66666666-6666-4666-8666-666666666666';
const registration: WorkerRegistration = {
    address: 'ws://worker-test:3131/',
    browser: 'chromium',
    max_slots: 5,
    playwright_version: '1.62.1',
};

class FakeGateway extends EventEmitter implements GatewayLike {
    readonly closedSessions: Array<{ sessionId: string; code: number }> = [];
    readonly browserEndpoints: string[] = [];
    shutdownCalled = false;
    private readonly sessions: string[];

    constructor(sessionIds: string[] = [], readonly listeningPort = 3131) {
        super();
        this.sessions = [...sessionIds];
    }

    setBrowserEndpoint(endpoint: string): void {
        this.browserEndpoints.push(endpoint);
    }

    get activeSessionIds(): string[] {
        return [...this.sessions];
    }

    get activeConnectionCount(): number {
        return this.sessions.length;
    }

    async start(): Promise<void> {}

    addSession(sessionId: string): void {
        this.sessions.push(sessionId);
    }

    closeSession(sessionId: string, code = 1001): void {
        const index = this.sessions.indexOf(sessionId);
        if (index >= 0) {
            this.sessions.splice(index, 1);
            this.closedSessions.push({ sessionId, code });
            this.emit('sessionClose', sessionId);
        }
    }

    async shutdown(): Promise<void> {
        this.shutdownCalled = true;
        this.sessions.splice(0);
    }
}

class HangingShutdownGateway extends FakeGateway {
    override async shutdown(): Promise<void> {
        await new Promise<void>(() => undefined);
    }
}

class FakeBrowser extends EventEmitter implements BrowserServerLike {
    killCalled = false;

    constructor(private readonly closeHangs = false) {
        super();
    }

    wsEndpoint(): string {
        return 'ws://127.0.0.1:12345/fake-browser';
    }

    async close(): Promise<void> {
        if (this.closeHangs) {
            await new Promise<void>(() => undefined);
        }
        this.emit('close');
    }

    async kill(): Promise<void> {
        this.killCalled = true;
        this.emit('close');
    }
}

// A browser that neither closes nor dies: close() hangs, kill() rejects.
class WedgedBrowser extends EventEmitter implements BrowserServerLike {
    wsEndpoint(): string {
        return 'ws://127.0.0.1:12345/wedged-browser';
    }

    async close(): Promise<void> {
        await new Promise<void>(() => undefined);
    }

    async kill(): Promise<void> {
        throw new Error('kill failed');
    }
}

class NetworkBrowser extends EventEmitter implements BrowserServerLike {
    private closed = false;

    constructor(
        private readonly server: WebSocketServer,
        private readonly endpoint: string,
    ) {
        super();
    }

    wsEndpoint(): string {
        return this.endpoint;
    }

    async close(): Promise<void> {
        if (this.closed) {
            return;
        }
        this.closed = true;
        await new Promise<void>(resolve => this.server.close(() => resolve()));
        this.emit('close');
    }

    async kill(): Promise<void> {
        for (const socket of this.server.clients) {
            socket.terminate();
        }
        await this.close();
    }
}

class FakeControlPlane implements ControlPlaneLike {
    readonly statuses: string[] = [];
    readonly registrations: WorkerRegistration[] = [];
    readonly recycles: string[] = [];

    constructor(
        private readonly maxLifetimeSessions = 0,
        private readonly recycledMaxLifetimeSessions = maxLifetimeSessions,
    ) {}

    async register(registration: WorkerRegistration): Promise<{ id: string; max_lifetime_sessions: number }> {
        this.registrations.push(registration);
        return { id: 'worker-1', max_lifetime_sessions: this.maxLifetimeSessions };
    }

    async recycle(workerId: string): Promise<{ id: string; max_lifetime_sessions: number }> {
        this.recycles.push(workerId);
        return { id: workerId, max_lifetime_sessions: this.recycledMaxLifetimeSessions };
    }

    async heartbeat(): Promise<{ status: 'available'; stale_session_ids: string[] }> {
        return { status: 'available', stale_session_ids: [] };
    }

    async setStatus(_workerId: string, status: 'draining' | 'shutting_down'): Promise<void> {
        this.statuses.push(status);
    }
}

function createWorker(serverUrl: string, gateway: FakeGateway): BrowserWorker {
    const config = testConfig(1_000);
    return new BrowserWorker(config, {
        controlPlane: new ControlPlaneClient(serverUrl, undefined, config.lifecycle.heartbeatIntervalMs),
        createGateway: () => gateway,
        launchBrowser: async () => new FakeBrowser(),
        exit: () => undefined,
    });
}

function createInjectedWorker(
    controlPlane: ControlPlaneLike,
    gateway: FakeGateway,
    drainTimeoutMs: number,
): BrowserWorker {
    return new BrowserWorker(testConfig(drainTimeoutMs), {
        controlPlane,
        createGateway: () => gateway,
        launchBrowser: async () => new FakeBrowser(),
        exit: () => undefined,
    });
}

function testConfig(drainTimeoutMs: number, port = 3131): WorkerConfig {
    return {
        controlPlane: { serverUrl: 'http://127.0.0.1:1' },
        browser: { type: 'chromium', headless: true },
        gateway: { port, privateHostname: 'worker-test', maxSlots: 5 },
        lifecycle: { heartbeatIntervalMs: 10, drainTimeoutMs },
        logging: { level: 'error', format: 'json' },
    };
}

async function startControlPlane(
    handler: (request: IncomingMessage, response: ServerResponse) => Promise<void>,
): Promise<{ url: string; close: () => Promise<void> }> {
    const server = http.createServer((request, response) => {
        void handler(request, response);
    });
    await new Promise<void>((resolve, reject) => {
        server.once('error', reject);
        server.listen(0, '127.0.0.1', resolve);
    });
    const address = server.address();
    if (!address || typeof address === 'string') {
        throw new Error('control-plane fixture did not use a TCP port');
    }
    return {
        url: `http://127.0.0.1:${address.port}`,
        close: () => new Promise(resolve => {
            server.close(() => resolve());
            server.closeAllConnections();
        }),
    };
}

async function readJSON<T>(request: IncomingMessage): Promise<T> {
    const chunks: Buffer[] = [];
    for await (const chunk of request) {
        chunks.push(Buffer.from(chunk));
    }
    return JSON.parse(Buffer.concat(chunks).toString()) as T;
}

function sendJSON(response: ServerResponse, status: number, body: unknown): void {
    response.writeHead(status, { 'Content-Type': 'application/json' });
    response.end(JSON.stringify(body));
}

function delay(delayMs: number): Promise<void> {
    return new Promise(resolve => setTimeout(resolve, delayMs));
}

async function waitFor(predicate: () => boolean, timeoutMs = 1_000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (!predicate()) {
        if (Date.now() >= deadline) {
            throw new Error('condition was not met before timeout');
        }
        await delay(5);
    }
}

function waitForWebSocketServer(server: WebSocketServer): Promise<void> {
    return new Promise((resolve, reject) => {
        server.once('listening', resolve);
        server.once('error', reject);
    });
}

function connectWebSocket(url: string, id: string): Promise<WebSocket> {
    return new Promise((resolve, reject) => {
        const socket = new WebSocket(url, { headers: { 'x-pwd-session-id': id } });
        socket.on('error', () => undefined);
        socket.once('open', () => resolve(socket));
        socket.once('error', reject);
    });
}

async function bounded<T>(operation: Promise<T>, timeoutMs: number): Promise<T> {
    return Promise.race([
        operation,
        delay(timeoutMs).then(() => {
            throw new Error(`operation did not finish within ${timeoutMs}ms`);
        }),
    ]);
}
