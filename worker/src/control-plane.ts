import { randomUUID } from 'node:crypto';
import { createClient, createConfig, type Client } from './api/client/index.js';
import {
    heartbeatWorker,
    recycleWorker,
    registerWorker,
    setWorkerStatus,
} from './api/sdk.gen.js';
import type {
    HeartbeatOutputBody,
    RegisteredWorker,
    RegisterWorkerInputBody,
    SetStatusInputBody,
    Worker,
} from './api/types.gen.js';

export class ControlPlaneError extends Error {
    constructor(message: string, readonly status?: number) {
        super(message);
        this.name = 'ControlPlaneError';
    }
}

type Sleep = (delayMs: number, signal?: AbortSignal) => Promise<void>;

const defaultRegistrationTimeoutMs = 5_000;
const statusTimeoutMs = 2_000;

export class ControlPlaneClient {
    private readonly client: Client;

    constructor(
        serverUrl: string,
        apiKey?: string,
        private readonly heartbeatTimeoutMs = 5_000,
        private readonly sleep: Sleep = abortableDelay,
        private readonly registrationTimeoutMs = defaultRegistrationTimeoutMs,
    ) {
        this.client = createClient(createConfig({
            baseUrl: serverUrl.replace(/\/$/, ''),
            headers: { Authorization: apiKey ? `Bearer ${apiKey}` : null },
        }));
    }

    async register(
        registration: RegisterWorkerInputBody,
        attempts = 30,
        retryDelayMs = 1000,
        signal?: AbortSignal,
    ): Promise<RegisteredWorker> {
        const body = { ...registration, instance_id: randomUUID() };
        let lastError: unknown;
        for (let attempt = 1; attempt <= attempts; attempt += 1) {
            signal?.throwIfAborted();
            try {
                const result = await registerWorker({
                    body,
                    client: this.client,
                    signal: requestSignal(this.registrationTimeoutMs, signal),
                });
                if (result.data) {
                    return result.data;
                }
                throw responseError('register worker', result.response, result.error);
            } catch (error) {
                lastError = error;
                if (signal?.aborted) {
                    signal.throwIfAborted();
                }
                if (isNonRetryableClientError(error)) {
                    throw error;
                }
                if (attempt < attempts) {
                    await this.sleep(retryDelayMs, signal);
                }
            }
        }
        throw new ControlPlaneError(
            `register worker failed after ${attempts} attempts: ${formatError(lastError)}`,
            lastError instanceof ControlPlaneError ? lastError.status : undefined,
        );
    }

    async recycle(workerId: string, signal?: AbortSignal): Promise<RegisteredWorker> {
        const result = await recycleWorker({
            path: { id: workerId },
            client: this.client,
            signal: requestSignal(statusTimeoutMs, signal),
        });
        if (result.data) {
            return result.data;
        }
        throw responseError('recycle worker', result.response, result.error);
    }

    async heartbeat(workerId: string, activeSessionIds: string[]): Promise<HeartbeatOutputBody> {
        const result = await heartbeatWorker({
            path: { id: workerId },
            body: { active_session_ids: activeSessionIds },
            client: this.client,
            signal: requestSignal(this.heartbeatTimeoutMs),
        });
        if (result.data) {
            return result.data;
        }
        throw responseError('heartbeat worker', result.response, result.error);
    }

    async setStatus(
        workerId: string,
        status: SetStatusInputBody['status'],
        signal?: AbortSignal,
    ): Promise<Worker> {
        const result = await setWorkerStatus({
            path: { id: workerId },
            body: { status },
            client: this.client,
            signal: requestSignal(statusTimeoutMs, signal),
        });
        if (result.data) {
            return result.data;
        }
        throw responseError('set worker status', result.response, result.error);
    }
}

function requestSignal(timeoutMs: number, signal?: AbortSignal): AbortSignal {
    const timeout = AbortSignal.timeout(timeoutMs);
    return signal ? AbortSignal.any([signal, timeout]) : timeout;
}

function isNonRetryableClientError(error: unknown): boolean {
    return error instanceof ControlPlaneError &&
        error.status !== undefined &&
        error.status >= 400 &&
        error.status < 500 &&
        error.status !== 408 &&
        error.status !== 429;
}

function abortableDelay(delayMs: number, signal?: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
        if (signal?.aborted) {
            reject(signal.reason);
            return;
        }
        const onAbort = () => {
            clearTimeout(timer);
            reject(signal!.reason);
        };
        const timer = setTimeout(() => {
            signal?.removeEventListener('abort', onAbort);
            resolve();
        }, delayMs);
        signal?.addEventListener('abort', onAbort, { once: true });
    });
}

function responseError(operation: string, response: Response | undefined, error: unknown): ControlPlaneError {
    const detail = typeof error === 'object' && error && 'detail' in error
        ? String(error.detail)
        : formatError(error);
    return new ControlPlaneError(
        `${operation} failed${response ? ` with HTTP ${response.status}` : ''}: ${detail}`,
        response?.status,
    );
}

function formatError(error: unknown): string {
    if (error instanceof Error) {
        return error.message;
    }
    if (typeof error === 'string') {
        return error;
    }
    try {
        return JSON.stringify(error);
    } catch {
        return String(error);
    }
}
