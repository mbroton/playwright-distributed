import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import http, { type IncomingMessage, type ServerResponse } from 'node:http';
import test from 'node:test';
import WebSocket, { WebSocketServer } from 'ws';
import { ControlPlaneClient, ControlPlaneError } from './control-plane.js';
import type { WorkerConfig } from './config.js';
import { WebSocketShim } from './shim.js';
import type {
    BrowserServerLike,
    ControlPlaneLike,
    ShimLike,
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
    await controlPlane.setStatus('worker-1', 'draining');

    assert.deepEqual(authorizationHeaders, [
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

    const shim = new FakeShim([activeId]);
    const worker = createWorker(server.url, shim);
    await worker.start();
    await waitFor(() => shim.closedSessions.length === 1);
    await delay(80);
    await worker.shutdown(0);

    assert.equal(maxConcurrentHeartbeats, 1);
    assert.deepEqual(heartbeatBodies[0], { active_session_ids: [activeId] });
    assert.deepEqual(shim.closedSessions, [{ sessionId: activeId, code: 1001 }]);
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

    const worker = createWorker(server.url, new FakeShim());
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

    const worker = createWorker(server.url, new FakeShim([activeId]));
    await worker.start();
    await waitFor(() => worker.state === 'draining');
    await worker.shutdown(0);

    assert.equal(worker.state, 'shutting_down');
});

test('a draining worker re-asserts drain intent after an available heartbeat', async () => {
    const controlPlane = new FakeControlPlane();
    const shim = new FakeShim([activeId]);
    const worker = createInjectedWorker(controlPlane, shim, 5_000);
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

    const worker = createWorker(server.url, new FakeShim());
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
    const worker = createInjectedWorker(controlPlane, new FakeShim([activeId]), 5_000);
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
    const shim = new FakeShim();
    const worker = createInjectedWorker(controlPlane, shim, 5_000);
    await worker.start();

    const startedAt = Date.now();
    await worker.requestDrain('SIGTERM', true);
    assert.equal(await worker.waitForExit(), 0);

    assert.ok(Date.now() - startedAt < 200);
    assert.deepEqual(controlPlane.statuses, ['draining', 'shutting_down']);
    assert.equal(shim.shutdownCalled, true);
});

test('drain timeout forces shutdown with active connections', async () => {
    const controlPlane = new FakeControlPlane();
    const shim = new FakeShim([activeId]);
    const worker = createInjectedWorker(controlPlane, shim, 30);
    await worker.start();

    await worker.requestDrain('control plane');
    assert.equal(await worker.waitForExit(), 0);

    assert.equal(shim.shutdownCalled, true);
    assert.ok(controlPlane.statuses.includes('draining'));
    assert.equal(controlPlane.statuses.at(-1), 'shutting_down');
});

test('drain timeout exits with a live session on the real shim', async t => {
    const browser = new WebSocketServer({ host: '127.0.0.1', port: 0 });
    await waitForWebSocketServer(browser);
    const address = browser.address();
    if (!address || typeof address === 'string') {
        throw new Error('browser fixture did not use a TCP port');
    }
    browser.on('connection', socket => socket.on('message', data => socket.send(data)));
    const browserServer = new NetworkBrowser(browser, `ws://127.0.0.1:${address.port}`);
    const controlPlane = new FakeControlPlane();
    let shim!: WebSocketShim;
    const worker = new BrowserWorker(testConfig(30, 0), {
        controlPlane,
        createShim: (endpoint, port) => {
            shim = new WebSocketShim(endpoint, port, '127.0.0.1');
            return shim;
        },
        launchBrowser: async () => browserServer,
        exit: () => undefined,
    });
    t.after(() => browserServer.kill());
    await worker.start();
    const client = await connectWebSocket(`ws://127.0.0.1:${shim.listeningPort}`, activeId);
    await waitFor(() => shim?.activeConnectionCount === 1);

    await worker.requestDrain('control plane');
    assert.equal(await bounded(worker.waitForExit(), 500), 0);
    assert.equal(worker.state, 'shutting_down');
    client.terminate();
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
        heartbeat: async () => {
            heartbeatCalls += 1;
            return { status: 'available', stale_session_ids: [] };
        },
        setStatus: async () => undefined,
    };
    const worker = createInjectedWorker(controlPlane, new FakeShim(), 5_000);
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
        createShim: () => new HangingShutdownShim(),
        launchBrowser: async () => new FakeBrowser(),
        exit: () => undefined,
        cleanupTimeoutMs: 20,
    });
    await worker.start();

    await worker.requestDrain('control plane');
    assert.equal(await bounded(worker.waitForExit(), 200), 0);
});

test('registration uses the port that the shim bound', async () => {
    const controlPlane = new FakeControlPlane();
    const worker = createInjectedWorker(controlPlane, new FakeShim([], 4321), 5_000);
    await worker.start();
    await worker.shutdown(0);

    assert.equal(controlPlane.registrations[0]?.address, 'ws://worker-test:4321/');
});

test('browser close timeout kills the browser before exit', async () => {
    const controlPlane = new FakeControlPlane();
    const shim = new FakeShim();
    const browser = new FakeBrowser(true);
    const worker = new BrowserWorker(testConfig(5_000), {
        controlPlane,
        createShim: () => shim,
        launchBrowser: async () => browser,
        exit: () => undefined,
        browserCloseTimeoutMs: 20,
    });
    await worker.start();

    await worker.requestDrain('control plane');
    assert.equal(await worker.waitForExit(), 0);
    assert.equal(browser.killCalled, true);
});

const activeId = '22222222-2222-4222-8222-222222222222';
const registration: WorkerRegistration = {
    address: 'ws://worker-test:3131/',
    browser: 'chromium',
    max_slots: 5,
    playwright_version: '1.62.1',
};

class FakeShim extends EventEmitter implements ShimLike {
    readonly closedSessions: Array<{ sessionId: string; code: number }> = [];
    shutdownCalled = false;
    private readonly sessions: string[];

    constructor(sessionIds: string[] = [], readonly listeningPort = 3131) {
        super();
        this.sessions = [...sessionIds];
    }

    get activeSessionIds(): string[] {
        return [...this.sessions];
    }

    get activeConnectionCount(): number {
        return this.sessions.length;
    }

    async start(): Promise<void> {}

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

class HangingShutdownShim extends FakeShim {
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

    async register(registration: WorkerRegistration): Promise<{ id: string }> {
        this.registrations.push(registration);
        return { id: 'worker-1' };
    }

    async heartbeat(): Promise<{ status: 'available'; stale_session_ids: string[] }> {
        return { status: 'available', stale_session_ids: [] };
    }

    async setStatus(_workerId: string, status: 'draining' | 'shutting_down'): Promise<void> {
        this.statuses.push(status);
    }
}

function createWorker(serverUrl: string, shim: FakeShim): BrowserWorker {
    const config = testConfig(1_000);
    return new BrowserWorker(config, {
        controlPlane: new ControlPlaneClient(serverUrl, undefined, config.lifecycle.heartbeatIntervalMs),
        createShim: () => shim,
        launchBrowser: async () => new FakeBrowser(),
        exit: () => undefined,
    });
}

function createInjectedWorker(
    controlPlane: ControlPlaneLike,
    shim: FakeShim,
    drainTimeoutMs: number,
): BrowserWorker {
    return new BrowserWorker(testConfig(drainTimeoutMs), {
        controlPlane,
        createShim: () => shim,
        launchBrowser: async () => new FakeBrowser(),
        exit: () => undefined,
    });
}

function testConfig(drainTimeoutMs: number, port = 3131): WorkerConfig {
    return {
        controlPlane: { serverUrl: 'http://127.0.0.1:1' },
        browser: { type: 'chromium', headless: true },
        shim: { port, privateHostname: 'worker-test', maxSlots: 5 },
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
