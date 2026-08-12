import assert from 'node:assert/strict';
import test from 'node:test';
import { ZodError } from 'zod';
import { parseConfig } from './config.js';

const requiredEnvironment = {
    REDIS_URL: 'redis://localhost:6379',
    PORT: '3131',
};

test('default timing keeps the heartbeat failure tolerance', () => {
    assert.doesNotThrow(() => parseConfig(requiredEnvironment));
});

test('converts second-based timing to the milliseconds the timers expect', () => {
    const config = parseConfig({
        ...requiredEnvironment,
        HEARTBEAT_INTERVAL: '10',
        REDIS_RETRY_DELAY: '3',
    });

    assert.equal(config.server.heartbeatInterval, 10 * 1000);
    assert.equal(config.redis.retryDelay, 3 * 1000);
});

test('accepts a worker key TTL three times the heartbeat interval', () => {
    assert.doesNotThrow(() => parseConfig({
        ...requiredEnvironment,
        HEARTBEAT_INTERVAL: '20',
        REDIS_KEY_TTL: '60',
    }));
});

test('rejects a worker key TTL without heartbeat failure tolerance', () => {
    assert.throws(
        () => parseConfig({
            ...requiredEnvironment,
            HEARTBEAT_INTERVAL: '21',
            REDIS_KEY_TTL: '60',
        }),
        (error: unknown) => {
            assert.ok(error instanceof ZodError);
            assert.equal(
                error.issues[0]?.message,
                'REDIS_KEY_TTL must be at least three times HEARTBEAT_INTERVAL'
            );
            return true;
        }
    );
});
