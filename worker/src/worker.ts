import { createRequire } from 'node:module';
import type { BrowserServer } from 'playwright-core';
import { chromium, firefox, webkit } from 'playwright-core';
import type { WorkerConfig } from './config.js';
import { ControlPlaneClient, ControlPlaneError } from './control-plane.js';
import { Logger } from './logger.js';
import { WebSocketShim } from './shim.js';

type WorkerState = 'starting' | 'running' | 'draining' | 'shutting_down';
type WorkerStatus = 'available' | 'draining' | 'stalled' | 'shutting_down';

export interface BrowserServerLike {
    wsEndpoint(): string;
    close(): Promise<void>;
    kill(): Promise<void>;
    once(event: 'close', listener: () => void): this;
}

export interface ShimLike {
    readonly activeSessionIds: string[];
    readonly activeConnectionCount: number;
    start(): Promise<void>;
    closeSession(sessionId: string, code?: number, reason?: string): void;
    shutdown(): Promise<void>;
    on(event: 'sessionClose', listener: (sessionId: string) => void): this;
}

export interface ControlPlaneLike {
    register(registration: WorkerRegistration, attempts?: number, retryDelayMs?: number): Promise<{ id: string }>;
    heartbeat(workerId: string, activeSessionIds: string[]): Promise<{
        status: WorkerStatus;
        stale_session_ids: string[];
    }>;
    setStatus(workerId: string, status: 'draining' | 'shutting_down', signal?: AbortSignal): Promise<unknown>;
}

export interface WorkerRegistration {
    address: string;
    browser: 'chromium' | 'firefox' | 'webkit';
    max_slots: number;
    playwright_version: string;
}

interface WorkerDependencies {
    controlPlane?: ControlPlaneLike;
    launchBrowser?: (config: WorkerConfig) => Promise<BrowserServerLike>;
    createShim?: (browserEndpoint: string, port: number) => ShimLike;
    logger?: Logger;
    exit?: (code: number) => void;
    browserCloseTimeoutMs?: number;
}

export class BrowserWorker {
    private currentState: WorkerState = 'starting';
    private workerIdValue: string | null = null;
    private browserServer: BrowserServerLike | null = null;
    private shim: ShimLike | null = null;
    private heartbeatTimer: NodeJS.Timeout | null = null;
    private heartbeatEnabled = false;
    private drainTimer: NodeJS.Timeout | null = null;
    private shutdownPromise: Promise<void> | null = null;
    private readonly controlPlane: ControlPlaneLike;
    private readonly launchBrowser: (config: WorkerConfig) => Promise<BrowserServerLike>;
    private readonly createShim: (browserEndpoint: string, port: number) => ShimLike;
    private readonly logger: Logger;
    private readonly exit: (code: number) => void;
    private readonly browserCloseTimeoutMs: number;
    private readonly exitPromise: Promise<number>;
    private resolveExit!: (code: number) => void;

    constructor(private readonly config: WorkerConfig, dependencies: WorkerDependencies = {}) {
        this.controlPlane = dependencies.controlPlane ?? new ControlPlaneClient(
            config.controlPlane.serverUrl,
            config.controlPlane.apiKey,
        );
        this.launchBrowser = dependencies.launchBrowser ?? launchPlaywrightServer;
        this.createShim = dependencies.createShim ?? ((endpoint, port) => new WebSocketShim(endpoint, port));
        this.logger = dependencies.logger ?? new Logger(
            config.shim.privateHostname,
            config.logging.level,
            config.logging.format,
        );
        this.exit = dependencies.exit ?? (code => process.exit(code));
        this.browserCloseTimeoutMs = dependencies.browserCloseTimeoutMs ?? 10_000;
        this.exitPromise = new Promise(resolve => {
            this.resolveExit = resolve;
        });
    }

    get state(): WorkerState {
        return this.currentState;
    }

    get workerId(): string | null {
        return this.workerIdValue;
    }

    waitForExit(): Promise<number> {
        return this.exitPromise;
    }

    async start(): Promise<void> {
        try {
            this.logger.info('Starting browser worker', {
                browserType: this.config.browser.type,
                port: this.config.shim.port,
                headless: this.config.browser.headless,
            });
            this.browserServer = await this.launchBrowser(this.config);
            this.browserServer.once('close', () => {
                if (this.currentState !== 'shutting_down') {
                    this.logger.error('Browser server exited unexpectedly');
                    void this.fatal(new Error('browser server exited unexpectedly'));
                }
            });

            this.shim = this.createShim(this.browserServer.wsEndpoint(), this.config.shim.port);
            this.shim.on('sessionClose', () => {
                if (this.currentState === 'draining' && this.shim?.activeConnectionCount === 0) {
                    void this.shutdown(0);
                }
            });
            await this.shim.start();
            await this.register();
            this.currentState = 'running';
            this.heartbeatEnabled = true;
            this.armHeartbeat();
            this.logger.info('Browser worker is ready', {
                workerId: this.workerIdValue,
                address: this.registration().address,
            });
        } catch (error) {
            this.logger.error('Worker startup failed', { error: formatError(error) });
            await this.shutdown(1);
        }
    }

    async requestDrain(reason: string, fromSignal = false): Promise<void> {
        if (this.currentState === 'shutting_down') {
            return;
        }
        if (this.currentState === 'draining') {
            this.logger.warn('Second drain request forces shutdown', { reason });
            await this.shutdown(0);
            return;
        }
        if (this.currentState === 'starting') {
            await this.shutdown(0);
            return;
        }

        this.currentState = 'draining';
        this.logger.info('Worker is draining', { reason });
        if (fromSignal && this.workerIdValue) {
            await this.bestEffortStatus('draining');
        }
        if (!this.shim || this.shim.activeConnectionCount === 0) {
            await this.shutdown(0);
            return;
        }
        this.drainTimer = setTimeout(() => {
            this.logger.warn('Drain timeout expired; forcing shutdown');
            void this.shutdown(0);
        }, this.config.lifecycle.drainTimeoutMs);
    }

    async fatal(error: unknown): Promise<void> {
        this.logger.error('Fatal worker error', { error: formatError(error) });
        await this.shutdown(1);
    }

    shutdown(exitCode: number): Promise<void> {
        if (this.shutdownPromise) {
            return this.shutdownPromise;
        }
        this.shutdownPromise = this.runShutdown(exitCode);
        return this.shutdownPromise;
    }

    private async register(): Promise<void> {
        const worker = await this.controlPlane.register(this.registration());
        this.workerIdValue = worker.id;
        this.logger.info('Worker registered', { workerId: worker.id });
    }

    private registration(): WorkerRegistration {
        return {
            address: `ws://${this.config.shim.privateHostname}:${this.config.shim.port}/`,
            browser: this.config.browser.type,
            max_slots: this.config.shim.maxSlots,
            playwright_version: playwrightVersion(),
        };
    }

    private armHeartbeat(): void {
        if (!this.heartbeatEnabled || this.currentState === 'shutting_down') {
            return;
        }
        this.heartbeatTimer = setTimeout(() => void this.heartbeatTick(), this.config.lifecycle.heartbeatIntervalMs);
        this.heartbeatTimer.unref();
    }

    private async heartbeatTick(): Promise<void> {
        if (!this.heartbeatEnabled || !this.workerIdValue || !this.shim) {
            return;
        }
        try {
            const response = await this.controlPlane.heartbeat(
                this.workerIdValue,
                this.shim.activeSessionIds,
            );
            if (!this.heartbeatEnabled || this.currentState === 'shutting_down') {
                return;
            }
            for (const sessionId of response.stale_session_ids) {
                this.shim.closeSession(sessionId, 1001, 'session is stale');
            }
            if (response.status === 'draining' && this.currentState !== 'draining') {
                await this.requestDrain('control plane');
            } else if (response.status === 'shutting_down') {
                await this.shutdown(0);
            }
        } catch (error) {
            if (error instanceof ControlPlaneError && error.status === 404) {
                this.logger.warn('Worker registration expired; registering again');
                try {
                    await this.register();
                } catch (registrationError) {
                    this.logger.warn('Worker re-registration failed', { error: formatError(registrationError) });
                }
            } else {
                this.logger.warn('Worker heartbeat failed', { error: formatError(error) });
            }
        } finally {
            this.armHeartbeat();
        }
    }

    private stopHeartbeat(): void {
        this.heartbeatEnabled = false;
        if (this.heartbeatTimer) {
            clearTimeout(this.heartbeatTimer);
            this.heartbeatTimer = null;
        }
    }

    private async runShutdown(exitCode: number): Promise<void> {
        this.currentState = 'shutting_down';
        this.stopHeartbeat();
        if (this.drainTimer) {
            clearTimeout(this.drainTimer);
            this.drainTimer = null;
        }
        this.logger.info('Worker is shutting down', { exitCode });

        try {
            await this.bestEffortStatus('shutting_down');
        } finally {
            try {
                if (this.shim) {
                    await this.runCleanup('shut down WebSocket shim', () => this.shim!.shutdown());
                }
            } finally {
                try {
                    if (this.browserServer) {
                        await this.closeBrowserServer(this.browserServer);
                    }
                } finally {
                    this.resolveExit(exitCode);
                    this.exit(exitCode);
                }
            }
        }
    }

    private async bestEffortStatus(status: 'draining' | 'shutting_down'): Promise<void> {
        if (!this.workerIdValue) {
            return;
        }
        await this.runCleanup(`set worker status to ${status}`, () => this.controlPlane.setStatus(
            this.workerIdValue!,
            status,
            AbortSignal.timeout(2_000),
        ).then(() => undefined));
    }

    private async closeBrowserServer(browserServer: BrowserServerLike): Promise<void> {
        let timer: NodeJS.Timeout | null = null;
        try {
            await Promise.race([
                browserServer.close(),
                new Promise<never>((_resolve, reject) => {
                    timer = setTimeout(() => reject(new Error('browser close timed out')), this.browserCloseTimeoutMs);
                }),
            ]);
        } catch (error) {
            this.logger.warn('Browser did not close cleanly; killing it', { error: formatError(error) });
            await this.runCleanup('kill browser server', () => browserServer.kill());
        } finally {
            if (timer) {
                clearTimeout(timer);
            }
        }
    }

    private async runCleanup(operation: string, cleanup: () => Promise<void>): Promise<void> {
        try {
            await cleanup();
        } catch (error) {
            this.logger.warn(`Failed to ${operation}`, { error: formatError(error) });
        }
    }
}

async function launchPlaywrightServer(config: WorkerConfig): Promise<BrowserServer> {
    const common = {
        host: '127.0.0.1',
        port: 0,
        headless: config.browser.headless,
        handleSIGINT: false,
        handleSIGTERM: false,
        handleSIGHUP: false,
    };
    switch (config.browser.type) {
        case 'chromium':
            return chromium.launchServer({ ...common, chromiumSandbox: true });
        case 'firefox':
            return firefox.launchServer(common);
        case 'webkit':
            return webkit.launchServer(common);
    }
}

function playwrightVersion(): string {
    const require = createRequire(import.meta.url);
    const packageJson = require('playwright-core/package.json') as { version: string };
    return packageJson.version;
}

function formatError(error: unknown): string {
    if (error instanceof Error) {
        return error.stack ?? error.message;
    }
    return String(error);
}
