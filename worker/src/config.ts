import { z } from 'zod';
import { config } from 'dotenv';

config();

const logLevels = ['debug', 'info', 'warn', 'error'] as const;
export type LogLevel = typeof logLevels[number];

export interface WorkerConfig {
    redis: {
        url: string;
        keyTtl: number; // in seconds
        retryAttempts: number;
        retryDelay: number; // in milliseconds
    };
    server: {
        port: number;
        privateHostname?: string | null;
        headless: boolean;
        heartbeatInterval: number; // in milliseconds
        browserType: 'chromium' | 'firefox' | 'webkit';
    };
    logging: {
        level: LogLevel;
        format: 'json' | 'text';
    };
}

const schema = z.object({
    REDIS_URL: z.url({ message: "Invalid Redis URL" }),
    
    // Time values from env are in seconds
    REDIS_KEY_TTL: z.coerce.number().int().positive().default(60),
    REDIS_RETRY_ATTEMPTS: z.coerce.number().int().positive().default(5),
    REDIS_RETRY_DELAY: z.coerce.number().int().positive().default(3),

    BROWSER_TYPE: z.enum(['chromium', 'firefox', 'webkit']).default('chromium'),
    PORT: z.coerce.number().int().positive(),
    PRIVATE_HOSTNAME: z.string().nullish(),
    HEADLESS: z.enum(['true', 'false']).default('true').transform(v => v === 'true'),
    HEARTBEAT_INTERVAL: z.coerce.number().int().positive().default(5),

    LOG_LEVEL: z.enum(logLevels).default('info'),
    LOG_FORMAT: z.enum(['json', 'text']).default('json'),
}).refine(
    values => values.HEARTBEAT_INTERVAL < values.REDIS_KEY_TTL,
    {
        message: 'HEARTBEAT_INTERVAL must be less than REDIS_KEY_TTL',
        path: ['HEARTBEAT_INTERVAL'],
    }
);

let loadedConfig: WorkerConfig | null = null;

export function parseConfig(environment: NodeJS.ProcessEnv): WorkerConfig {
    const parsed = schema.parse(environment);
    return {
        redis: {
            url: parsed.REDIS_URL,
            keyTtl: parsed.REDIS_KEY_TTL,
            retryAttempts: parsed.REDIS_RETRY_ATTEMPTS,
            retryDelay: parsed.REDIS_RETRY_DELAY * 1000, // Converted to MS for setTimeout
        },
        server: {
            browserType: parsed.BROWSER_TYPE,
            port: parsed.PORT,
            privateHostname: parsed.PRIVATE_HOSTNAME,
            headless: parsed.HEADLESS,
            heartbeatInterval: parsed.HEARTBEAT_INTERVAL * 1000, // Converted to MS for setInterval
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
            console.error('Configuration validation failed:', error.issues);
            process.exit(1);
        }
        throw error;
    }
}
