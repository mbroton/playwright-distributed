import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import http, { type IncomingMessage, type ServerResponse } from 'node:http';
import test from 'node:test';
import { ControlPlaneClient } from './control-plane.js';
import type { WorkerConfig } from './config.js';
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
            await delay(40);
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
    assert.deepEqual(controlPlane.statuses, ['shutting_down']);
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

    constructor(sessionIds: string[] = []) {
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

class FakeControlPlane implements ControlPlaneLike {
    readonly statuses: string[] = [];

    async register(_registration: WorkerRegistration): Promise<{ id: string }> {
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
    return new BrowserWorker(testConfig(1_000), {
        controlPlane: new ControlPlaneClient(serverUrl),
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

function testConfig(drainTimeoutMs: number): WorkerConfig {
    return {
        controlPlane: { serverUrl: 'http://127.0.0.1:1' },
        browser: { type: 'chromium', headless: true },
        shim: { port: 3131, privateHostname: 'worker-test', maxSlots: 5 },
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
        close: () => new Promise(resolve => server.close(() => resolve())),
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
