import { chromium, firefox, webkit, type BrowserServer } from 'playwright-core';
import { createClient } from 'redis';
import { loadConfig } from './config.js';
import type { WorkerConfig } from './config.js';
import { Logger } from './logger.js';


interface WorkerMetadata {
    id: string;
    browserType: 'chromium' | 'firefox' | 'webkit';
    endpoint: string;
    status: 'available' | 'draining' | 'shutting-down';
    startedAt: number;
    lastHeartbeat: number;
}

const clusterActiveConnectionsKey = 'cluster:active_connections';
const clusterLifetimeConnectionsKey = 'cluster:lifetime_connections';
type RedisClient = ReturnType<typeof createClient>;


class BrowserWorker {
    private readonly workerId: string;
    private readonly config: WorkerConfig;
    private readonly logger: Logger;
    private browserServer: BrowserServer | null = null;

    private readonly redis: RedisClient;
    private readonly redisSub: RedisClient;
    private readonly redisKey: string;
    private readonly redisCmdKey: string;
    private readonly workerIdKey: string;

    // State
    private isShuttingDown: boolean = false;
    private isDraining: boolean = false;
    private isRunning: boolean = false;
    private isRestoringRedisState: boolean = false;
    private internalEndpoint: string | null = null;
    private startedAt: number | null = null;

    // Timers
    private heartbeatTimer: NodeJS.Timeout | null = null;
    private drainTimer: NodeJS.Timeout | null = null;
    private drainTimeout: NodeJS.Timeout | null = null;

    constructor() {
        this.workerId = crypto.randomUUID();
        this.config = loadConfig();
        this.logger = new Logger(this.workerId, this.config.logging.level, this.config.logging.format);
        this.redis = this.createRedisClient('main');
        this.redisSub = this.createRedisClient('subscription');
        this.redisKey = `worker:${this.config.server.browserType}:${this.workerId}`;
        this.redisCmdKey = `worker:cmd:${this.config.server.browserType}:${this.workerId}`;
        this.workerIdKey = `${this.config.server.browserType}:${this.workerId}`;
    }

    private formatError(error: unknown): Record<string, any> {
        if (error instanceof Error) {
            return { message: error.message, stack: error.stack };
        }
        return { message: String(error) };
    }

    private createRedisClient(purpose: string): RedisClient {
        let hasConnected = false;
        const client = createClient({
            url: this.config.redis.url,
            disableOfflineQueue: true,
            socket: {
                connectTimeout: 2000,
                reconnectStrategy: (retries, cause) => {
                    if (retries + 1 >= this.config.redis.retryAttempts) {
                        return new Error(
                            `Redis ${purpose} reconnect attempts exhausted: ${cause.message}`
                        );
                    }
                    return this.config.redis.retryDelay;
                }
            }
        });

        client.on('error', (error) => {
            this.logger.warn(`Redis ${purpose} client error`, {
                error: this.formatError(error)
            });

            if (this.isRunning && !this.isShuttingDown && !client.isOpen) {
                void this.gracefulShutdown(`redis_${purpose}_reconnect_exhausted`, 1);
            }
        });

        client.on('reconnecting', () => {
            this.logger.warn(`Redis ${purpose} client reconnecting`);
        });

        client.on('ready', () => {
            if (!hasConnected) {
                hasConnected = true;
                this.logger.debug(`Redis ${purpose} client connected`);
                return;
            }

            this.logger.info(`Redis ${purpose} client reconnected`);
            if (purpose === 'main' && this.isRunning && !this.isShuttingDown) {
                void this.restoreRedisState();
            }
        });

        client.on('end', () => {
            this.logger.debug(`Redis ${purpose} client closed`);
        });

        return client;
    }

    private async connectRedisClient(client: RedisClient, purpose: string): Promise<void> {
        this.logger.debug(`Connecting to Redis for ${purpose}...`);
        await client.connect();
        await client.ping();
        this.logger.debug(`Successfully connected to Redis for ${purpose}`);
    }

    private async listenForCommands(): Promise<void> {
        this.logger.debug('Setting up command subscription...', { channel: this.redisCmdKey });

        try {
            await this.redisSub.subscribe(this.redisCmdKey, (message) => {
                this.logger.debug('Received command via Pub/Sub', { message, channel: this.redisCmdKey });

                if (message === 'shutdown') {
                    this.logger.debug('Shutdown command received. Initiating drain...');
                    this.drainAndShutdown('shutdown_command_pubsub');
                }
            });

            this.logger.debug('Successfully subscribed to command channel', { channel: this.redisCmdKey });
        } catch (error) {
            this.logger.error('Failed to subscribe to command channel', {
                channel: this.redisCmdKey,
                error: this.formatError(error)
            });
            throw error;
        }
    }

    public async start(): Promise<void> {
        this.logger.info('Starting browser worker...', {
            browserType: this.config.server.browserType,
            port: this.config.server.port,
            headless: this.config.server.headless
        });

        try {
            await Promise.all([
                this.connectRedisClient(this.redis, 'main'),
                this.connectRedisClient(this.redisSub, 'subscription')
            ]);

            await this.listenForCommands();

            const browserConfig = {
                port: this.config.server.port,
                headless: this.config.server.headless,
                wsPath: `/playwright/${this.workerId}`,
            };
            
            switch (this.config.server.browserType) {
                case 'chromium':
                    this.browserServer = await chromium.launchServer({
                        ...browserConfig,
                        chromiumSandbox: true
                    });
                    break;
                case 'firefox':
                    this.browserServer = await firefox.launchServer(browserConfig);
                    break;
                case 'webkit':
                    this.browserServer = await webkit.launchServer(browserConfig);
                    break;
                default:
                    throw new Error(`Unknown browser type: ${this.config.server.browserType}`);
            };

            const wsEndpoint = this.browserServer.wsEndpoint();
            this.internalEndpoint = wsEndpoint;
            if (this.config.server.privateHostname) {
                this.internalEndpoint = wsEndpoint.replace(/ws:\/\/127\.0\.0\.1|ws:\/\/localhost/, `ws://${this.config.server.privateHostname}`);
            }

            this.logger.debug('Browser server launched', { endpoint: this.internalEndpoint });

            await this.initializeCounters();
            await this.register();

            this.isRunning = true;
            this.startHeartbeat();

            process.on('SIGINT', () => this.gracefulShutdown('SIGINT'));
            process.on('SIGTERM', () => this.gracefulShutdown('SIGTERM'));

            this.logger.info('Browser worker is ready to work');

        } catch (error) {
            this.logger.error('Failed to start browser worker', { error: this.formatError(error) });
            await this.cleanupAndExit(1);
        }
    }

    private async restoreRedisState(): Promise<void> {
        if (this.isRestoringRedisState || this.isShuttingDown || !this.redis.isReady) {
            return;
        }

        this.isRestoringRedisState = true;
        try {
            await this.initializeCounters();
            await this.register();
            this.logger.info('Redis state restored after reconnect');
        } catch (error) {
            this.logger.error('Failed to restore Redis state after reconnect', {
                error: this.formatError(error)
            });
        } finally {
            this.isRestoringRedisState = false;
        }
    }

    private async initializeCounters(): Promise<void> {
        this.logger.debug('Initializing worker connection counters in Redis...');
        try {
            const [activeResult, lifetimeResult] = await Promise.all([
                this.redis.hSetNX(clusterActiveConnectionsKey, this.workerIdKey, String(0)),
                this.redis.hSetNX(clusterLifetimeConnectionsKey, this.workerIdKey, String(0))
            ]);

            if (activeResult && lifetimeResult) {
                this.logger.debug('Successfully initialized connection counters.');
            } else {
                this.logger.debug('Connection counters were already initialized for this worker.');
            }
        } catch (error) {
            this.logger.error('Failed to initialize connection counters. Worker will not start.', { error: this.formatError(error) });
            throw error;
        }
    }

    private async register(): Promise<void> {
        if (!this.internalEndpoint) {
            this.logger.error('No endpoint available for registration. Aborting.');
            await this.gracefulShutdown('registration_error_no_endpoint');
            return;
        }

        if (!this.startedAt) {
            this.startedAt = Date.now()
        }

        const metadata: WorkerMetadata = {
            id: this.workerId,
            browserType: this.config.server.browserType,
            endpoint: this.internalEndpoint,
            status: 'available',
            startedAt: this.startedAt,
            lastHeartbeat: Date.now()
        };

        await this.redis.hSet(this.redisKey, metadata as unknown as Record<string, string>);
        await this.redis.expire(this.redisKey, this.config.redis.keyTtl);

        this.logger.debug('Worker registered in Redis', { key: this.redisKey, endpoint: this.internalEndpoint });
    }

    private startHeartbeat(): void {
        this.heartbeatTimer = setInterval(
            () => this.performHeartbeat(),
            this.config.server.heartbeatInterval
        );
        this.logger.debug('Heartbeat started', { intervalMs: this.config.server.heartbeatInterval });
    }

    private async performHeartbeat(): Promise<void> {
        if (this.isShuttingDown || this.isRestoringRedisState || !this.redis.isReady) return;

        try {
            const exists = await this.redis.exists(this.redisKey);
            if (!exists) {
                this.logger.warn('Worker key expired. Re-registering...');
                await this.initializeCounters();
                await this.register();
                return;
            }

            await this.redis.hSet(this.redisKey, 'lastHeartbeat', Date.now());
            await this.redis.expire(this.redisKey, this.config.redis.keyTtl);
            this.logger.debug('Heartbeat sent', { key: this.redisKey });


            if (this.isDraining) {
                return
            }

            const command = await this.redis.get(this.redisCmdKey);
            if (command === 'shutdown') {
                this.logger.info('Shutdown command received. Initiating drain...');
                await this.redis.del(this.redisCmdKey);
                await this.drainAndShutdown('shutdown_command');
                return;
            }

        } catch (error) {
            this.logger.warn('Failed to perform heartbeat', { error: this.formatError(error) });
            if (!this.redis.isOpen) {
                await this.gracefulShutdown('redis_main_reconnect_exhausted', 1);
            }
        }
    }

    private async drainAndShutdown(initiator: string): Promise<void> {
        if (this.isDraining || this.isShuttingDown) {
            return;
        }

        this.isDraining = true;
        this.logger.info('Starting drain process...', { initiator });

        try {
            await this.redis.hSet(this.redisKey, 'status', 'draining');
        } catch (error) {
            this.logger.error('Failed to update worker status to draining', { error: this.formatError(error) });
        }

        this.drainTimeout = setTimeout(() => {
            this.logger.warn('Drain timeout reached. Forcing shutdown.');
            this.gracefulShutdown('drain_timeout');
        }, 5 * 60 * 1000);

        this.drainTimer = setInterval(async () => {
            try {
                const activeConnections = await this.redis.hGet(clusterActiveConnectionsKey, this.workerIdKey);
                const count = activeConnections ? parseInt(activeConnections, 10) : 0;

                if (!Number.isFinite(count) || count < 0) {
                    this.logger.warn('Invalid connection count from Redis, treating as 0', { rawValue: activeConnections });
                    if (this.drainTimeout) {
                        clearTimeout(this.drainTimeout);
                        this.drainTimeout = null;
                    }
                    this.logger.warn('Invalid connection count detected. Proceeding with shutdown.');
                    await this.gracefulShutdown('drain_invalid_count');
                    return;
                }

                this.logger.info(`Draining... ${count} active connections remaining.`);

                if (count === 0) {
                    if (this.drainTimeout) {
                        clearTimeout(this.drainTimeout);
                        this.drainTimeout = null;
                    }
                    this.logger.info('No active connections remaining. Proceeding with shutdown.');
                    await this.gracefulShutdown('drain_complete');
                }
            } catch (error) {
                this.logger.error('Error checking active connections during drain', { error: this.formatError(error) });
                if (this.drainTimeout) {
                    clearTimeout(this.drainTimeout);
                    this.drainTimeout = null;
                }
                await this.gracefulShutdown('drain_error');
            }
        }, 1000);
    }

    private async gracefulShutdown(initiator: string, exitCode: number = 0): Promise<void> {
        if (this.isShuttingDown) return;
        this.isShuttingDown = true;
        this.isDraining = false;

        this.logger.info('Initiating graceful shutdown...', { initiator });

        if (this.heartbeatTimer) {
            clearInterval(this.heartbeatTimer);
            this.heartbeatTimer = null;
        }
        if (this.drainTimer) {
            clearInterval(this.drainTimer);
            this.drainTimer = null;
        }
        if (this.drainTimeout) {
            clearTimeout(this.drainTimeout);
            this.drainTimeout = null;
        }

        try {
            if (this.redis.isReady) {
                this.logger.debug('Updating worker status to "shutting-down" in Redis.');
                const exists = await this.redis.exists(this.redisKey);
                if (exists) {
                    await this.redis.hSet(this.redisKey, 'status', 'shutting-down');
                    await this.redis.expire(this.redisKey, 10);
                }
            }
        } catch (error) {
            this.logger.error('Failed to update worker status during shutdown.', { error: this.formatError(error) });
        }

        if (this.browserServer) {
            this.logger.debug('Closing the browser server.');
            await this.browserServer.close();
            this.logger.info('Browser server closed.');
        }

        await this.cleanupAndExit(exitCode);
    }

    private async closeRedisClient(client: RedisClient): Promise<void> {
        if (!client.isOpen) {
            return;
        }

        if (client.isReady) {
            await client.quit();
            return;
        }

        await client.disconnect();
    }

    private async cleanupAndExit(exitCode: number): Promise<void> {
        if (this.heartbeatTimer) {
            clearInterval(this.heartbeatTimer);
        }
        if (this.drainTimer) {
            clearInterval(this.drainTimer);
        }
        if (this.drainTimeout) {
            clearTimeout(this.drainTimeout);
        }

        try {
            if (this.redis.isReady) {
                await this.redis.del(this.redisKey);
                this.logger.debug('Worker key removed from Redis.');
            }
        } catch (error) {
            this.logger.error('Failed to remove worker key from Redis during cleanup.', { error: this.formatError(error) });
        }

        try {
            await Promise.all([
                this.closeRedisClient(this.redis),
                this.closeRedisClient(this.redisSub)
            ]);
            this.logger.debug('Redis connections closed.');
        } catch (error) {
            this.logger.error('Failed to close Redis connections during cleanup.', { error: this.formatError(error) });
        }

        this.logger.info('Worker cleanup complete. Exiting process.', { exitCode });
        process.exit(exitCode);
    }
}

if (process.argv[1] === new URL(import.meta.url).pathname) {
    const worker = new BrowserWorker();
    worker.start().catch(async (error) => {
        console.error(JSON.stringify({
            timestamp: new Date().toISOString(),
            level: 'error',
            message: 'Caught unhandled exception during startup. Exiting.',
            error: error instanceof Error ? { message: error.message, stack: error.stack } : { message: String(error) }
        }));
        process.exit(1);
    });
}
