import { client } from './api/client.gen.js';
import {
    heartbeatWorker,
    registerWorker,
    setWorkerStatus,
} from './api/sdk.gen.js';
import type {
    HeartbeatOutputBody,
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

type Sleep = (delayMs: number) => Promise<void>;

export class ControlPlaneClient {
    constructor(
        serverUrl: string,
        apiKey?: string,
        private readonly sleep: Sleep = delay => new Promise(resolve => setTimeout(resolve, delay)),
    ) {
        client.setConfig({
            baseUrl: serverUrl.replace(/\/$/, ''),
            headers: { Authorization: apiKey ? `Bearer ${apiKey}` : null },
        });
    }

    async register(
        registration: RegisterWorkerInputBody,
        attempts = 30,
        retryDelayMs = 1000,
    ): Promise<Worker> {
        let lastError: unknown;
        for (let attempt = 1; attempt <= attempts; attempt += 1) {
            try {
                const result = await registerWorker({ body: registration });
                if (result.data) {
                    return result.data;
                }
                throw responseError('register worker', result.response, result.error);
            } catch (error) {
                lastError = error;
                if (attempt < attempts) {
                    await this.sleep(retryDelayMs);
                }
            }
        }
        throw new ControlPlaneError(
            `register worker failed after ${attempts} attempts: ${formatError(lastError)}`,
            lastError instanceof ControlPlaneError ? lastError.status : undefined,
        );
    }

    async heartbeat(workerId: string, activeSessionIds: string[]): Promise<HeartbeatOutputBody> {
        const result = await heartbeatWorker({
            path: { id: workerId },
            body: { active_session_ids: activeSessionIds },
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
            signal,
        });
        if (result.data) {
            return result.data;
        }
        throw responseError('set worker status', result.response, result.error);
    }
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
