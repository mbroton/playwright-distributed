import { createRequire } from 'node:module';
import type { BrowserServer } from 'playwright-core';
import { chromium, firefox, webkit } from 'playwright-core';
import type { WorkerConfig } from './config.js';
import { ControlPlaneClient, ControlPlaneError } from './control-plane.js';
import { Logger } from './logger.js';
import { SessionGateway } from './gateway.js';

type WorkerState = 'starting' | 'running' | 'draining' | 'shutting_down';
type WorkerStatus = 'available' | 'draining' | 'stalled' | 'shutting_down';

export interface BrowserServerLike {
    wsEndpoint(): string;
    close(): Promise<void>;
    kill(): Promise<void>;
    once(event: 'close', listener: () => void): this;
}

export interface GatewayLike {
    readonly activeSessionIds: string[];
    readonly activeConnectionCount: number;
    readonly listeningPort: number | null;
    start(): Promise<void>;
    setBrowserEndpoint(endpoint: string): void;
    closeSession(sessionId: string, code?: number, reason?: string): void;
    shutdown(): Promise<void>;
    on(event: 'sessionClose', listener: (sessionId: string) => void): this;
}

export interface ControlPlaneLike {
    register(
        registration: WorkerRegistration,
        attempts?: number,
        retryDelayMs?: number,
        signal?: AbortSignal,
    ): Promise<{ id: string; max_lifetime_sessions?: number }>;
    recycle(
        workerId: string,
        attempts?: number,
        retryDelayMs?: number,
        signal?: AbortSignal,
    ): Promise<{
        id: string;
        max_lifetime_sessions?: number;
    }>;
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
    createGateway?: (browserEndpoint: string, port: number) => GatewayLike;
    logger?: Logger;
    exit?: (code: number) => void;
    browserCloseTimeoutMs?: number;
    cleanupTimeoutMs?: number;
}

export class BrowserWorker {
    private currentState: WorkerState = 'starting';
    private workerIdValue: string | null = null;
    private browserServer: BrowserServerLike | null = null;
    private gateway: GatewayLike | null = null;
    private heartbeatTimer: NodeJS.Timeout | null = null;
    private heartbeatEnabled = false;
    private drainTimer: NodeJS.Timeout | null = null;
    private recycleOnDrained = false;
    private recyclePromise: Promise<void> | null = null;
    private recycleGeneration = 0;
    private registerPromise: Promise<void> | null = null;
    private sessionBudget = 0;
    private servedSessions = 0;
    private shutdownPromise: Promise<void> | null = null;
    private readonly shutdownController = new AbortController();
    private readonly controlPlane: ControlPlaneLike;
    private readonly launchBrowser: (config: WorkerConfig) => Promise<BrowserServerLike>;
    private readonly createGateway: (browserEndpoint: string, port: number) => GatewayLike;
    private readonly logger: Logger;
    private readonly exit: (code: number) => void;
    private readonly browserCloseTimeoutMs: number;
    private readonly cleanupTimeoutMs: number;
    private readonly exitPromise: Promise<number>;
    private resolveExit!: (code: number) => void;

    constructor(private readonly config: WorkerConfig, dependencies: WorkerDependencies = {}) {
        this.controlPlane = dependencies.controlPlane ?? new ControlPlaneClient(
            config.controlPlane.serverUrl,
            config.controlPlane.apiKey,
            Math.min(config.lifecycle.heartbeatIntervalMs, 5_000),
        );
        this.launchBrowser = dependencies.launchBrowser ?? launchPlaywrightServer;
        this.createGateway = dependencies.createGateway ?? ((endpoint, port) => new SessionGateway(endpoint, port));
        this.logger = dependencies.logger ?? new Logger(
            config.gateway.privateHostname,
            config.logging.level,
            config.logging.format,
        );
        this.exit = dependencies.exit ?? (code => process.exit(code));
        this.browserCloseTimeoutMs = dependencies.browserCloseTimeoutMs ?? 10_000;
        this.cleanupTimeoutMs = dependencies.cleanupTimeoutMs ?? 2_500;
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
                port: this.config.gateway.port,
                headless: this.config.browser.headless,
            });
            const browserServer = await this.launchBrowser(this.config);
            if (this.shutdownStarted) {
                await this.closeBrowserServer(browserServer);
                return;
            }
            this.attachBrowserServer(browserServer);

            this.gateway = this.createGateway(browserServer.wsEndpoint(), this.config.gateway.port);
            this.gateway.on('sessionClose', () => {
                this.servedSessions += 1;
                if (this.currentState === 'draining' && this.gateway?.activeConnectionCount === 0) {
                    void this.drainCompleted();
                    return;
                }
                // Drain without waiting for the heartbeat to say so: until the
                // recycled worker becomes available, the scheduler has already
                // stopped selecting this one, so every waiting second is dead
                // capacity on small deployments. This counts gateway closes
                // while the server counts claims, so a claim that never
                // reaches the gateway leaves this counter short — the
                // heartbeat's draining status stays as the backstop.
                if (
                    this.currentState === 'running' &&
                    this.sessionBudget > 0 &&
                    this.servedSessions >= this.sessionBudget
                ) {
                    void this.requestDrain('session budget spent');
                }
            });
            await this.gateway.start();
            if (this.shutdownStarted) {
                return;
            }
            await this.register();
            if (this.shutdownStarted) {
                return;
            }
            this.currentState = 'running';
            this.heartbeatEnabled = true;
            this.armHeartbeat();
            this.logger.info('Browser worker is ready', {
                workerId: this.workerIdValue,
                address: this.registration().address,
            });
        } catch (error) {
            if (this.shutdownStarted) {
                return;
            }
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
        // A control-plane drain means "give me a fresh browser" (the shipped
        // compose files restart exited workers anyway). Recycling in place
        // avoids the supervisor's crash-loop backoff, which grows into tens
        // of seconds of downtime when sessions turn over quickly. A signal
        // drain is a real stop request and still exits.
        this.recycleOnDrained = !fromSignal;
        this.logger.info('Worker is draining', { reason, recycleOnDrained: this.recycleOnDrained });
        if (fromSignal && this.workerIdValue) {
            await this.bestEffortStatus('draining');
        }
        if (!this.gateway || this.gateway.activeConnectionCount === 0) {
            await this.drainCompleted();
            return;
        }
        this.drainTimer = setTimeout(() => {
            this.logger.warn('Drain timeout expired; forcing the drain to complete');
            void this.drainCompleted();
        }, this.config.lifecycle.drainTimeoutMs);
    }

    private async drainCompleted(): Promise<void> {
        if (this.currentState !== 'draining') {
            return;
        }
        if (this.drainTimer) {
            clearTimeout(this.drainTimer);
            this.drainTimer = null;
        }
        if (this.recycleOnDrained) {
            await this.recycle();
        } else {
            await this.shutdown(0);
        }
    }

    private recycle(): Promise<void> {
        this.recyclePromise ??= this.runRecycle().finally(() => {
            this.recyclePromise = null;
        });
        return this.recyclePromise;
    }

    private async runRecycle(): Promise<void> {
        if (this.shutdownStarted) {
            return;
        }
        // First of two bumps (the second is at completion): heartbeats
        // dispatched before this point must not act after the swap.
        this.recycleGeneration += 1;
        this.currentState = 'starting';
        this.logger.info('Recycling the browser in place');
        // A forced recycle (drain timeout) can still have live sessions;
        // close them first so the old browser shuts down promptly.
        for (const sessionId of this.gateway?.activeSessionIds ?? []) {
            this.gateway?.closeSession(sessionId, 1001, 'worker is recycling');
        }
        const oldServer = this.browserServer;
        this.browserServer = null;
        try {
            const workerId = this.workerIdValue;
            if (!workerId) {
                throw new Error('worker has no registration to recycle');
            }
            // Launch the replacement before closing the old browser: sessions
            // that dial the gateway mid-recycle reach a live endpoint instead
            // of a dead one, at the cost of two browsers briefly coexisting.
            const browserServer = await this.launchBrowser(this.config);
            if (this.shutdownStarted) {
                await this.closeBrowserServer(browserServer);
                if (oldServer) {
                    await this.closeBrowserServer(oldServer);
                }
                return;
            }
            this.attachBrowserServer(browserServer);
            this.gateway?.setBrowserEndpoint(browserServer.wsEndpoint());
            // Reset at the swap, not after register(): sessions served by the
            // replacement browser while the old one is still closing must
            // count toward the new budget, not be erased by a later reset.
            this.servedSessions = 0;
            if (oldServer && !(await this.closeBrowserServer(oldServer))) {
                // A wedged browser we cannot stop would leak a process per
                // recycle; exit instead so the supervisor reclaims them all.
                throw new Error('old browser could not be stopped');
            }
            try {
                const recycled = await this.controlPlane.recycle(
                    workerId,
                    undefined,
                    undefined,
                    this.shutdownController.signal,
                );
                this.shutdownController.signal.throwIfAborted();
                this.workerIdValue = recycled.id;
                this.sessionBudget = recycled.max_lifetime_sessions ?? 0;
            } catch (error) {
                if (!(error instanceof ControlPlaneError)) {
                    throw error;
                }
                if (error.status === 404) {
                    this.logger.warn('Worker row expired during recycle; registering again');
                    await this.register();
                } else if (error.status === 409) {
                    this.logger.info('Worker shutdown intent prevents recycle; shutting down');
                    await this.shutdown(0);
                    return;
                } else {
                    throw error;
                }
            }
            // Second bump: heartbeats keep flowing during the swap (they keep
            // the row alive through a long recycle retry) and carry the
            // already-bumped generation, so only a bump at completion makes
            // their late responses distinguishable from post-swap ones.
            this.recycleGeneration += 1;
            this.currentState = 'running';
            this.logger.info('Worker recycled with a fresh browser', { workerId: this.workerIdValue });
        } catch (error) {
            if (this.shutdownStarted) {
                return;
            }
            this.logger.error('Recycle failed; exiting for a supervisor restart', { error: formatError(error) });
            await this.shutdown(1);
        }
    }

    private attachBrowserServer(browserServer: BrowserServerLike): void {
        this.browserServer = browserServer;
        browserServer.once('close', () => {
            if (this.browserServer === browserServer && this.currentState !== 'shutting_down') {
                this.logger.error('Browser server exited unexpectedly');
                void this.fatal(new Error('browser server exited unexpectedly'));
            }
        });
    }

    async fatal(error: unknown): Promise<void> {
        this.logger.error('Fatal worker error', { error: formatError(error) });
        await this.shutdown(1);
    }

    shutdown(exitCode: number): Promise<void> {
        if (this.shutdownPromise) {
            return this.shutdownPromise;
        }
        this.shutdownController.abort(new Error('worker shutdown started'));
        this.shutdownPromise = this.runShutdown(exitCode);
        return this.shutdownPromise;
    }

    private register(): Promise<void> {
        // Single-flight: a heartbeat-404 re-registration can overlap a
        // recycle's own register fallback, and each call mints a fresh
        // instance id — two concurrent calls would create two server rows
        // for one gateway.
        this.registerPromise ??= this.runRegister().finally(() => {
            this.registerPromise = null;
        });
        return this.registerPromise;
    }

    private async runRegister(): Promise<void> {
        const worker = await this.controlPlane.register(
            this.registration(),
            undefined,
            undefined,
            this.shutdownController.signal,
        );
        this.shutdownController.signal.throwIfAborted();
        this.workerIdValue = worker.id;
        this.sessionBudget = worker.max_lifetime_sessions ?? 0;
        // servedSessions is deliberately NOT reset here: a heartbeat-404
        // re-registration keeps the same browser, so its spent budget must
        // keep counting. Only a recycle (fresh browser) resets the counter.
        this.logger.info('Worker registered', { workerId: worker.id, sessionBudget: this.sessionBudget });
    }

    private registration(): WorkerRegistration {
        const port = this.gateway?.listeningPort ?? this.config.gateway.port;
        return {
            address: `ws://${this.config.gateway.privateHostname}:${port}/`,
            browser: this.config.browser.type,
            max_slots: this.config.gateway.maxSlots,
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
        if (!this.heartbeatEnabled || !this.workerIdValue || !this.gateway) {
            this.armHeartbeat();
            return;
        }
        // A heartbeat response can arrive during or after a browser swap.
        // The generation rejects responses from an earlier recycle even when
        // the worker ID and running state are unchanged after the swap.
        const workerId = this.workerIdValue;
        const recycleGeneration = this.recycleGeneration;
        try {
            const response = await this.controlPlane.heartbeat(
                workerId,
                this.gateway.activeSessionIds,
            );
            if (
                !this.heartbeatEnabled ||
                this.currentState === 'starting' ||
                this.currentState === 'shutting_down' ||
                this.workerIdValue !== workerId ||
                this.recycleGeneration !== recycleGeneration
            ) {
                return;
            }
            for (const sessionId of response.stale_session_ids) {
                this.gateway.closeSession(sessionId, 1001, 'session is stale');
            }
            if (response.status === 'shutting_down') {
                await this.shutdown(0);
            } else if (this.currentState === 'draining') {
                if (response.status !== 'draining') {
                    await this.bestEffortStatus('draining');
                }
            } else if (response.status === 'draining') {
                await this.requestDrain('control plane');
            }
        } catch (error) {
            if (
                this.currentState === 'starting' ||
                this.workerIdValue !== workerId ||
                this.recycleGeneration !== recycleGeneration
            ) {
                // A recycle owns lifecycle changes during and after its browser swap.
            } else if (error instanceof ControlPlaneError && error.status === 404) {
                this.logger.warn('Worker registration expired; registering again');
                try {
                    await this.register();
                    if (this.currentState === 'draining') {
                        await this.bestEffortStatus('draining');
                    }
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
                if (this.gateway) {
                    await this.runCleanup('shut down WebSocket gateway', () => this.gateway!.shutdown());
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

    private async bestEffortStatus(
        status: 'draining' | 'shutting_down',
        workerId: string | null = this.workerIdValue,
    ): Promise<void> {
        if (!workerId) {
            return;
        }
        await this.runCleanup(`set worker status to ${status}`, () => this.controlPlane.setStatus(
            workerId,
            status,
            AbortSignal.timeout(2_000),
        ).then(() => undefined));
    }

    private async closeBrowserServer(browserServer: BrowserServerLike): Promise<boolean> {
        let timer: NodeJS.Timeout | null = null;
        try {
            await Promise.race([
                browserServer.close(),
                new Promise<never>((_resolve, reject) => {
                    timer = setTimeout(() => reject(new Error('browser close timed out')), this.browserCloseTimeoutMs);
                }),
            ]);
            return true;
        } catch (error) {
            this.logger.warn('Browser did not close cleanly; killing it', { error: formatError(error) });
            return this.runCleanup('kill browser server', () => browserServer.kill());
        } finally {
            if (timer) {
                clearTimeout(timer);
            }
        }
    }

    private async runCleanup(operation: string, cleanup: () => Promise<void>): Promise<boolean> {
        let timer: NodeJS.Timeout | null = null;
        try {
            await Promise.race([
                cleanup(),
                new Promise<never>((_resolve, reject) => {
                    timer = setTimeout(
                        () => reject(new Error(`${operation} timed out`)),
                        this.cleanupTimeoutMs,
                    );
                }),
            ]);
            return true;
        } catch (error) {
            this.logger.warn(`Failed to ${operation}`, { error: formatError(error) });
            return false;
        } finally {
            if (timer) {
                clearTimeout(timer);
            }
        }
    }

    private get shutdownStarted(): boolean {
        return this.shutdownController.signal.aborted;
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
