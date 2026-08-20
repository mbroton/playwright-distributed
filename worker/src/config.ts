import os from 'node:os';
import { config as loadDotenv } from 'dotenv';
import { z } from 'zod';

loadDotenv({ quiet: true });

const logLevels = ['debug', 'info', 'warn', 'error'] as const;
export type LogLevel = typeof logLevels[number];

export interface WorkerConfig {
    controlPlane: {
        serverUrl: string;
        apiKey?: string;
    };
    browser: {
        type: 'chromium' | 'firefox' | 'webkit';
        headless: boolean;
    };
    shim: {
        port: number;
        privateHostname: string;
        maxSlots: number;
    };
    lifecycle: {
        heartbeatIntervalMs: number;
        drainTimeoutMs: number;
    };
    logging: {
        level: LogLevel;
        format: 'json' | 'text';
    };
}

const schema = z.object({
    SERVER_URL: z.url({ protocol: /^https?$/, message: 'SERVER_URL must be an http(s) URL' }),
    WORKER_API_KEY: z.string().min(1).optional(),
    BROWSER_TYPE: z.enum(['chromium', 'firefox', 'webkit']).default('chromium'),
    PORT: z.coerce.number().int().min(1).max(65535).default(3131),
    PRIVATE_HOSTNAME: z.string().min(1).optional(),
    MAX_SLOTS: z.coerce.number().int().min(1).default(5),
    HEADLESS: z.enum(['true', 'false']).default('true').transform(value => value === 'true'),
    HEARTBEAT_INTERVAL: z.coerce.number().int().min(1).default(5),
    DRAIN_TIMEOUT: z.coerce.number().int().min(0).default(300),
    LOG_LEVEL: z.enum(logLevels).default('info'),
    LOG_FORMAT: z.enum(['json', 'text']).default('json'),
});

let loadedConfig: WorkerConfig | null = null;

export function parseConfig(environment: NodeJS.ProcessEnv): WorkerConfig {
    const parsed = schema.parse(environment);
    return {
        controlPlane: {
            serverUrl: parsed.SERVER_URL,
            ...(parsed.WORKER_API_KEY && { apiKey: parsed.WORKER_API_KEY }),
        },
        browser: {
            type: parsed.BROWSER_TYPE,
            headless: parsed.HEADLESS,
        },
        shim: {
            port: parsed.PORT,
            // A container hostname is its unique, routable container ID. This
            // lets Docker Compose scale workers without fixed hostnames.
            privateHostname: parsed.PRIVATE_HOSTNAME ?? os.hostname(),
            maxSlots: parsed.MAX_SLOTS,
        },
        lifecycle: {
            heartbeatIntervalMs: parsed.HEARTBEAT_INTERVAL * 1000,
            drainTimeoutMs: parsed.DRAIN_TIMEOUT * 1000,
        },
        logging: {
            level: parsed.LOG_LEVEL,
            format: parsed.LOG_FORMAT,
        },
    };
}

export function loadConfig(): WorkerConfig {
    if (loadedConfig) {
        return loadedConfig;
    }

    try {
        loadedConfig = parseConfig(process.env);
        return loadedConfig;
    } catch (error) {
        if (error instanceof z.ZodError) {
            console.error('Configuration validation failed:');
            for (const issue of error.issues) {
                console.error(`- ${issue.path.join('.') || 'environment'}: ${issue.message}`);
            }
            process.exit(1);
        }
        throw error;
    }
}
