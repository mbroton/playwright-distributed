import { EventEmitter } from 'node:events';
import http, { type IncomingHttpHeaders, type IncomingMessage } from 'node:http';
import type { Duplex } from 'node:stream';
import WebSocket, { WebSocketServer, type RawData } from 'ws';

interface SessionSockets {
    client: WebSocket;
    upstream: WebSocket;
}

interface ShimEvents {
    sessionClose: [sessionId: string];
}

const sessionHeader = 'x-pwd-session-id';
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

export class WebSocketShim extends EventEmitter<ShimEvents> {
    private readonly server = http.createServer((_request, response) => {
        response.writeHead(426).end('WebSocket upgrade required');
    });
    private readonly webSocketServer = new WebSocketServer({ noServer: true });
    private readonly sessions = new Map<string, SessionSockets>();
    private readonly pendingSessionIds = new Set<string>();
    private readonly pendingSessions = new Map<string, SessionSockets>();
    private shuttingDown = false;
    private listenPort: number | null = null;

    constructor(
        private readonly browserEndpoint: string,
        private readonly port: number,
        private readonly host = '0.0.0.0',
    ) {
        super();
        this.server.on('upgrade', (request, socket, head) => {
            this.handleUpgrade(request, socket, head);
        });
    }

    get activeSessionIds(): string[] {
        return [...this.sessions.keys()];
    }

    get activeConnectionCount(): number {
        return this.sessions.size;
    }

    get listeningPort(): number {
        if (this.listenPort === null) {
            throw new Error('WebSocket shim is not listening');
        }
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
                    reject(new Error('WebSocket shim has no TCP address'));
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

    closeSession(sessionId: string, code = 1001, reason = 'session closed'): void {
        const session = this.sessions.get(sessionId);
        if (!session) {
            return;
        }
        closeSocket(session.client, code, reason);
        closeSocket(session.upstream, code, reason);
        this.removeSession(sessionId);
    }

    async shutdown(): Promise<void> {
        this.shuttingDown = true;
        await new Promise<void>((resolve, reject) => {
            if (!this.server.listening) {
                resolve();
                return;
            }
            this.server.close(error => error ? reject(error) : resolve());
        });
        for (const sessionId of this.activeSessionIds) {
            this.closeSession(sessionId, 1001, 'worker shutting down');
        }
        for (const [sessionId, session] of this.pendingSessions) {
            closeSocket(session.client, 1001, 'worker shutting down');
            closeSocket(session.upstream, 1001, 'worker shutting down');
            this.pendingSessions.delete(sessionId);
            this.pendingSessionIds.delete(sessionId);
        }
        this.webSocketServer.close();
    }

    private handleUpgrade(request: IncomingMessage, socket: Duplex, head: Buffer): void {
        const sessionId = singleHeader(request.headers[sessionHeader]);
        if (!sessionId || !uuidPattern.test(sessionId)) {
            rejectUpgrade(socket, 400, 'Invalid x-pwd-session-id');
            return;
        }
        if (this.pendingSessionIds.has(sessionId) || this.sessions.has(sessionId)) {
            rejectUpgrade(socket, 409, 'Session is already connected');
            return;
        }
        if (this.shuttingDown) {
            rejectUpgrade(socket, 503, 'Worker is shutting down');
            return;
        }

        this.pendingSessionIds.add(sessionId);
        this.webSocketServer.handleUpgrade(request, socket, head, client => {
            this.connectUpstream(sessionId, request, client);
        });
    }

    private connectUpstream(sessionId: string, request: IncomingMessage, client: WebSocket): void {
        const upstream = new WebSocket(this.browserEndpoint, {
            headers: forwardedHeaders(request.headers),
        });
        this.pendingSessions.set(sessionId, { client, upstream });
        const failBeforeOpen = () => {
            this.pendingSessionIds.delete(sessionId);
            this.pendingSessions.delete(sessionId);
            closeSocket(client, 1011, 'browser connection failed');
        };
        upstream.once('error', failBeforeOpen);
        client.once('close', failBeforeOpen);

        upstream.once('open', () => {
            upstream.off('error', failBeforeOpen);
            client.off('close', failBeforeOpen);
            this.pendingSessionIds.delete(sessionId);
            this.pendingSessions.delete(sessionId);
            if (client.readyState !== WebSocket.OPEN || this.shuttingDown) {
                closeSocket(upstream, 1001, 'client disconnected');
                closeSocket(client, 1001, 'worker shutting down');
                return;
            }
            this.sessions.set(sessionId, { client, upstream });
            this.pipeSession(sessionId, client, upstream);
        });
    }

    private pipeSession(sessionId: string, client: WebSocket, upstream: WebSocket): void {
        forwardMessages(client, upstream, () => this.closeSession(sessionId, 1011, 'relay write failed'));
        forwardMessages(upstream, client, () => this.closeSession(sessionId, 1011, 'relay write failed'));

        client.once('close', (code, reason) => this.closePeer(sessionId, upstream, code, reason));
        upstream.once('close', (code, reason) => this.closePeer(sessionId, client, code, reason));
        client.once('error', () => this.closeSession(sessionId, 1011, 'client connection failed'));
        upstream.once('error', () => this.closeSession(sessionId, 1011, 'browser connection failed'));
    }

    private closePeer(sessionId: string, peer: WebSocket, code: number, reason: Buffer): void {
        const outgoingCode = isLegalCloseCode(code) ? code : 1011;
        const outgoingReason = isLegalCloseCode(code) ? reason : Buffer.from('peer connection failed');
        closeSocket(peer, outgoingCode, outgoingReason);
        this.removeSession(sessionId);
    }

    private removeSession(sessionId: string): void {
        if (this.sessions.delete(sessionId)) {
            this.emit('sessionClose', sessionId);
        }
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
    let pendingWrites = 0;
    source.on('message', (data: RawData, isBinary: boolean) => {
        if (target.readyState !== WebSocket.OPEN) {
            onError();
            return;
        }
        pendingWrites += 1;
        source.pause();
        target.send(data, { binary: isBinary }, error => {
            pendingWrites -= 1;
            if (error) {
                onError();
                return;
            }
            if (pendingWrites === 0) {
                source.resume();
            }
        });
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

function singleHeader(value: string | string[] | undefined): string | undefined {
    return typeof value === 'string' ? value : undefined;
}

function isLegalCloseCode(code: number): boolean {
    return (code >= 1000 && code <= 1014 && ![1004, 1005, 1006].includes(code)) ||
        (code >= 3000 && code <= 4999);
}
