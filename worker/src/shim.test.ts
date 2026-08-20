import assert from 'node:assert/strict';
import type { IncomingMessage } from 'node:http';
import test from 'node:test';
import WebSocket, { WebSocketServer } from 'ws';
import { WebSocketShim } from './shim.js';

const sessionId = '11111111-1111-4111-8111-111111111111';

test('relays text and binary frames and tracks the session map', async t => {
    const fixture = await createFixture();
    t.after(() => fixture.close());

    const client = await connect(fixture.shimUrl, sessionId);
    await waitFor(() => fixture.shim.activeConnectionCount === 1);
    assert.deepEqual(fixture.shim.activeSessionIds, [sessionId]);
    assert.equal(fixture.shim.activeConnectionCount, 1);

    const textMessage = nextMessage(client);
    client.send('hello');
    assert.deepEqual(await textMessage, { data: Buffer.from('hello'), isBinary: false });

    const binaryMessage = nextMessage(client);
    client.send(Buffer.from([0, 1, 2, 255]));
    assert.deepEqual(await binaryMessage, {
        data: Buffer.from([0, 1, 2, 255]),
        isBinary: true,
    });

    client.close(1000, 'done');
    await waitForClose(client);
    await waitFor(() => fixture.shim.activeConnectionCount === 0);
    assert.deepEqual(fixture.shim.activeSessionIds, []);
});

test('rejects missing, invalid, and duplicate session IDs', async t => {
    const fixture = await createFixture();
    t.after(() => fixture.close());

    assert.equal(await refusedStatus(fixture.shimUrl), 400);
    assert.equal(await refusedStatus(fixture.shimUrl, 'not-a-uuid'), 400);

    const first = await connect(fixture.shimUrl, sessionId);
    assert.equal(await refusedStatus(fixture.shimUrl, sessionId), 409);
    first.close();
    await waitForClose(first);
});

test('forwards only Playwright headers and User-Agent', async t => {
    const fixture = await createFixture();
    t.after(() => fixture.close());

    const client = await connect(fixture.shimUrl, sessionId, {
        Authorization: 'Bearer secret',
        Cookie: 'secret=cookie',
        'User-Agent': 'Playwright/1.62.1',
        'x-playwright-proxy': 'enabled',
        'x-unrelated': 'drop-me',
    });
    const request = await fixture.nextUpstreamRequest();

    assert.equal(request.headers['user-agent'], 'Playwright/1.62.1');
    assert.equal(request.headers['x-playwright-proxy'], 'enabled');
    assert.equal(request.headers.authorization, undefined);
    assert.equal(request.headers.cookie, undefined);
    assert.equal(request.headers['x-unrelated'], undefined);

    client.close();
    await waitForClose(client);
});

test('a client close closes the browser-side socket', async t => {
    const fixture = await createFixture();
    t.after(() => fixture.close());

    const client = await connect(fixture.shimUrl, sessionId);
    const upstream = await fixture.nextUpstreamSocket();
    const upstreamClosed = waitForClose(upstream);

    client.close(1000, 'client done');
    await waitForClose(client);
    const close = await upstreamClosed;
    assert.equal(close.code, 1000);
    assert.equal(close.reason.toString(), 'client done');
    assert.equal(fixture.shim.activeConnectionCount, 0);
});

async function createFixture(): Promise<{
    shim: WebSocketShim;
    shimUrl: string;
    nextUpstreamRequest: () => Promise<IncomingMessage>;
    nextUpstreamSocket: () => Promise<WebSocket>;
    close: () => Promise<void>;
}> {
    const browser = new WebSocketServer({ host: '127.0.0.1', port: 0 });
    await waitForListening(browser);
    const browserAddress = browser.address();
    if (!browserAddress || typeof browserAddress === 'string') {
        throw new Error('fake browser did not use a TCP port');
    }

    const requests: IncomingMessage[] = [];
    const requestWaiters: Array<(request: IncomingMessage) => void> = [];
    const sockets: WebSocket[] = [];
    const socketWaiters: Array<(socket: WebSocket) => void> = [];
    browser.on('connection', (socket, request) => {
        const requestWaiter = requestWaiters.shift();
        if (requestWaiter) {
            requestWaiter(request);
        } else {
            requests.push(request);
        }
        const socketWaiter = socketWaiters.shift();
        if (socketWaiter) {
            socketWaiter(socket);
        } else {
            sockets.push(socket);
        }
        socket.on('message', (data, isBinary) => socket.send(data, { binary: isBinary }));
    });

    const shim = new WebSocketShim(`ws://127.0.0.1:${browserAddress.port}`, 0, '127.0.0.1');
    await shim.start();

    return {
        shim,
        shimUrl: `ws://127.0.0.1:${shim.listeningPort}`,
        nextUpstreamRequest: () => {
            const request = requests.shift();
            return request ? Promise.resolve(request) : new Promise(resolve => requestWaiters.push(resolve));
        },
        nextUpstreamSocket: () => {
            const socket = sockets.shift();
            return socket ? Promise.resolve(socket) : new Promise(resolve => socketWaiters.push(resolve));
        },
        close: async () => {
            await shim.shutdown();
            for (const socket of browser.clients) {
                socket.terminate();
            }
            await new Promise<void>(resolve => browser.close(() => resolve()));
        },
    };
}

function connect(url: string, id: string, headers: Record<string, string> = {}): Promise<WebSocket> {
    return new Promise((resolve, reject) => {
        const socket = new WebSocket(url, {
            headers: { ...headers, 'x-pwd-session-id': id },
        });
        socket.once('open', () => resolve(socket));
        socket.once('error', reject);
    });
}

function refusedStatus(url: string, id?: string): Promise<number> {
    return new Promise((resolve, reject) => {
        const socket = new WebSocket(url, {
            headers: id ? { 'x-pwd-session-id': id } : {},
        });
        socket.once('unexpected-response', (_request, response) => {
            response.resume();
            resolve(response.statusCode ?? 0);
        });
        socket.once('open', () => reject(new Error('WebSocket upgrade was accepted')));
        socket.on('error', () => undefined);
    });
}

function nextMessage(socket: WebSocket): Promise<{ data: Buffer; isBinary: boolean }> {
    return new Promise((resolve, reject) => {
        socket.once('message', (data, isBinary) => resolve({ data: Buffer.from(data as Buffer), isBinary }));
        socket.once('error', reject);
    });
}

function waitForClose(socket: WebSocket): Promise<{ code: number; reason: Buffer }> {
    if (socket.readyState === WebSocket.CLOSED) {
        return Promise.resolve({ code: 1006, reason: Buffer.alloc(0) });
    }
    return new Promise(resolve => {
        socket.once('close', (code, reason) => resolve({ code, reason }));
    });
}

function waitForListening(server: WebSocketServer): Promise<void> {
    return new Promise((resolve, reject) => {
        server.once('listening', resolve);
        server.once('error', reject);
    });
}

async function waitFor(predicate: () => boolean, timeoutMs = 1_000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (!predicate()) {
        if (Date.now() >= deadline) {
            throw new Error('condition was not met before timeout');
        }
        await new Promise(resolve => setTimeout(resolve, 5));
    }
}
