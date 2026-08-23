import { EventEmitter } from 'node:events';
import http, { type IncomingHttpHeaders, type IncomingMessage } from 'node:http';
import type { Duplex } from 'node:stream';
import WebSocket, { WebSocketServer, type RawData } from 'ws';

interface SessionSockets {
    client: WebSocket;
    upstream: WebSocket;
}

interface PendingSession {
    clientSocket: Duplex;
    upstream: WebSocket;
}

interface GatewayEvents {
    sessionClose: [sessionId: string];
}

const sessionHeader = 'x-pwd-session-id';
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
// Must stay below the server's WORKER_DIAL_TIMEOUT (default 10s): the relay's
// dial now spans this browser handshake, and the inner deadline must lose the
// race so the failure surfaces as the gateway's 502, not a relay dial timeout.
const upstreamHandshakeTimeoutMs = 7_000;
const shutdownTimeoutMs = 1_000;
const backpressureHighWaterMark = 1024 * 1024;

export class SessionGateway extends EventEmitter<GatewayEvents> {
    private readonly server = http.createServer((_request, response) => {
        response.writeHead(426).end('WebSocket upgrade required');
    });
    private readonly webSocketServer = new WebSocketServer({ noServer: true });
    private readonly sessions = new Map<string, SessionSockets>();
    private readonly pendingSessionIds = new Set<string>();
    private readonly pendingSessions = new Map<string, PendingSession>();
    private shuttingDown = false;
    private shutdownPromise: Promise<void> | null = null;
    private listenPort: number | null = null;

    constructor(
        private browserEndpoint: string,
        private readonly port: number,
        private readonly host = '0.0.0.0',
    ) {
        super();
        this.server.on('upgrade', (request, socket, head) => {
            this.handleUpgrade(request, socket, head);
        });
    }

    // Recycling swaps the browser under a running gateway: new sessions dial
    // the fresh endpoint, established sessions keep their upstream sockets.
    setBrowserEndpoint(endpoint: string): void {
        this.browserEndpoint = endpoint;
    }

    get activeSessionIds(): string[] {
        return [...this.sessions.keys()];
    }

    get activeConnectionCount(): number {
        return this.sessions.size;
    }

    get listeningPort(): number | null {
        return this.listenPort;
    }

    async start(): Promise<void> {
        await new Promise<void>((resolve, reject) => {
            const onError = (error: Error) => {
                this.server.off('listening', onListening);
                reject(error);
            };
            const onListening = () => {
                this.server.off('error', onError);
                const address = this.server.address();
                if (!address || typeof address === 'string') {
                    reject(new Error('WebSocket gateway has no TCP address'));
                    return;
                }
                this.listenPort = address.port;
                resolve();
            };
            this.server.once('error', onError);
            this.server.once('listening', onListening);
            this.server.listen(this.port, this.host);
        });
    }

    closeSession(
        sessionId: string,
        code = 1001,
        reason = 'session closed',
        expectedSession = this.sessions.get(sessionId),
    ): void {
        if (!expectedSession || this.sessions.get(sessionId) !== expectedSession) {
            return;
        }
        closeSocket(expectedSession.client, code, reason);
        closeSocket(expectedSession.upstream, code, reason);
        this.removeSession(sessionId, expectedSession);
    }

    shutdown(): Promise<void> {
        if (!this.shutdownPromise) {
            this.shutdownPromise = this.runShutdown();
        }
        return this.shutdownPromise;
    }

    private async runShutdown(): Promise<void> {
        this.shuttingDown = true;
        const sessionSockets = [...this.sessions.values()];
        for (const sessionId of this.activeSessionIds) {
            this.closeSession(sessionId, 1001, 'worker shutting down');
        }
        for (const [sessionId, session] of this.pendingSessions) {
            this.pendingSessions.delete(sessionId);
            this.pendingSessionIds.delete(sessionId);
            session.upstream.terminate();
            if (!session.clientSocket.destroyed) {
                rejectUpgrade(session.clientSocket, 503, 'Worker is shutting down');
            }
        }

        const closed = Promise.all([
            closeWebSocketServer(this.webSocketServer),
            closeHttpServer(this.server),
        ]).then(() => undefined);
        this.server.closeAllConnections();
        await waitWithDeadline(closed, shutdownTimeoutMs, () => {
            for (const session of sessionSockets) {
                session.client.terminate();
                session.upstream.terminate();
            }
            this.server.closeAllConnections();
        });
    }

    private handleUpgrade(request: IncomingMessage, socket: Duplex, head: Buffer): void {
        socket.on('error', () => socket.destroy());
        const sessionId = singleHeader(request.headers[sessionHeader]);
        if (!sessionId || !uuidPattern.test(sessionId)) {
            rejectUpgrade(socket, 400, 'Invalid x-pwd-session-id');
            return;
        }
        if (this.shuttingDown) {
            rejectUpgrade(socket, 503, 'Worker is shutting down');
            return;
        }
        if (this.pendingSessionIds.has(sessionId) || this.sessions.has(sessionId)) {
            rejectUpgrade(socket, 409, 'Session is already connected');
            return;
        }

        this.pendingSessionIds.add(sessionId);
        let upstream: WebSocket;
        try {
            // Node validates forwarded header values synchronously inside the
            // WebSocket constructor; a throw here must not reach uncaughtException.
            upstream = new WebSocket(this.browserEndpoint, {
                headers: forwardedHeaders(request.headers),
                handshakeTimeout: upstreamHandshakeTimeoutMs,
            });
        } catch {
            this.pendingSessionIds.delete(sessionId);
            rejectUpgrade(socket, 502, 'Browser connection failed');
            return;
        }
        let acceptedSession: SessionSockets | undefined;
        upstream.on('error', () => {
            if (this.pendingSessions.get(sessionId)?.upstream === upstream) {
                rejectClient(502, 'Browser connection failed');
            } else if (acceptedSession) {
                this.closeSession(sessionId, 1011, 'browser connection failed', acceptedSession);
            }
        });
        this.pendingSessions.set(sessionId, { clientSocket: socket, upstream });

        const clearPending = (): boolean => {
            if (this.pendingSessions.get(sessionId)?.upstream !== upstream) {
                return false;
            }
            this.pendingSessionIds.delete(sessionId);
            this.pendingSessions.delete(sessionId);
            socket.off('close', abortDial);
            upstream.off('open', acceptClient);
            return true;
        };
        const abortDial = () => {
            if (clearPending()) {
                upstream.terminate();
            }
        };
        const rejectClient = (status: number, message: string) => {
            if (!clearPending()) {
                return;
            }
            upstream.terminate();
            if (!socket.destroyed) {
                rejectUpgrade(socket, status, message);
            }
        };
        const acceptClient = () => {
            if (this.shuttingDown) {
                rejectClient(503, 'Worker is shutting down');
                return;
            }
            if (socket.destroyed) {
                abortDial();
                return;
            }
            try {
                this.webSocketServer.handleUpgrade(request, socket, head, client => {
                    const session = { client, upstream };
                    acceptedSession = session;
                    client.once('error', () => this.closeSession(
                        sessionId,
                        1011,
                        'client connection failed',
                        session,
                    ));
                    this.sessions.set(sessionId, session);
                    this.pipeSession(sessionId, session);
                    clearPending();
                });
            } catch {
                rejectClient(400, 'Invalid WebSocket upgrade');
            }
        };

        socket.once('close', abortDial);
        upstream.once('open', acceptClient);
    }

    private pipeSession(sessionId: string, session: SessionSockets): void {
        const { client, upstream } = session;
        forwardMessages(client, upstream, () => this.closeSession(sessionId, 1011, 'relay write failed', session));
        forwardMessages(upstream, client, () => this.closeSession(sessionId, 1011, 'relay write failed', session));

        client.once('close', (code, reason) => this.closePeer(sessionId, session, upstream, code, reason));
        upstream.once('close', (code, reason) => this.closePeer(sessionId, session, client, code, reason));
    }

    private closePeer(
        sessionId: string,
        expectedSession: SessionSockets,
        peer: WebSocket,
        code: number,
        reason: Buffer,
    ): void {
        if (this.sessions.get(sessionId) !== expectedSession) {
            return;
        }
        const outgoingCode = isLegalCloseCode(code) ? code : 1011;
        const outgoingReason = isLegalCloseCode(code) ? reason : Buffer.from('peer connection failed');
        closeSocket(peer, outgoingCode, outgoingReason);
        this.removeSession(sessionId, expectedSession);
    }

    private removeSession(sessionId: string, expectedSession: SessionSockets): void {
        if (this.sessions.get(sessionId) !== expectedSession) {
            return;
        }
        this.sessions.delete(sessionId);
        this.emit('sessionClose', sessionId);
    }
}

function forwardedHeaders(headers: IncomingHttpHeaders): IncomingHttpHeaders {
    const forwarded: IncomingHttpHeaders = {};
    const userAgent = headers['user-agent'];
    if (userAgent !== undefined) {
        forwarded['user-agent'] = userAgent;
    }
    for (const [name, value] of Object.entries(headers)) {
        if (name.toLowerCase().startsWith('x-playwright-') && value !== undefined) {
            forwarded[name] = value;
        }
    }
    return forwarded;
}

function forwardMessages(source: WebSocket, target: WebSocket, onError: () => void): void {
    let paused = false;
    source.on('message', (data: RawData, isBinary: boolean) => {
        if (target.readyState !== WebSocket.OPEN) {
            onError();
            return;
        }
        target.send(data, { binary: isBinary }, error => {
            if (error) {
                onError();
                return;
            }
            if (paused && target.bufferedAmount <= backpressureHighWaterMark) {
                paused = false;
                source.resume();
            }
        });
        if (!paused && target.bufferedAmount > backpressureHighWaterMark) {
            paused = true;
            source.pause();
        }
    });
}

function closeSocket(socket: WebSocket, code: number, reason: string | Buffer): void {
    if (socket.readyState === WebSocket.OPEN) {
        socket.close(code, reason);
        return;
    }
    if (socket.readyState === WebSocket.CONNECTING) {
        socket.terminate();
    }
}

function rejectUpgrade(socket: Duplex, status: number, message: string): void {
    socket.end(
        `HTTP/1.1 ${status} ${http.STATUS_CODES[status] ?? 'Error'}\r\n` +
        'Connection: close\r\n' +
        'Content-Type: text/plain; charset=utf-8\r\n' +
        `Content-Length: ${Buffer.byteLength(message)}\r\n\r\n` +
        message,
    );
}

function closeWebSocketServer(server: WebSocketServer): Promise<void> {
    return new Promise(resolve => server.close(() => resolve()));
}

function closeHttpServer(server: http.Server): Promise<void> {
    return new Promise((resolve, reject) => {
        if (!server.listening) {
            resolve();
            return;
        }
        server.close(error => error ? reject(error) : resolve());
    });
}

async function waitWithDeadline(
    operation: Promise<void>,
    timeoutMs: number,
    onTimeout: () => void,
): Promise<void> {
    let timer: NodeJS.Timeout | null = null;
    try {
        await Promise.race([
            operation,
            new Promise<void>(resolve => {
                timer = setTimeout(() => {
                    onTimeout();
                    resolve();
                }, timeoutMs);
            }),
        ]);
    } finally {
        if (timer) {
            clearTimeout(timer);
        }
    }
}

function singleHeader(value: string | string[] | undefined): string | undefined {
    return typeof value === 'string' ? value : undefined;
}

function isLegalCloseCode(code: number): boolean {
    return (code >= 1000 && code <= 1014 && ![1004, 1005, 1006].includes(code)) ||
        (code >= 3000 && code <= 4999);
}
