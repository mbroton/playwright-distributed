import assert from 'node:assert/strict';
import test from 'node:test';
import { ZodError } from 'zod';
import { parseConfig } from './config.js';

const requiredEnvironment = {
    REDIS_URL: 'redis://localhost:6379',
    PORT: '3131',
};

test('uses safe timing defaults', () => {
    const config = parseConfig(requiredEnvironment);

    assert.equal(config.server.heartbeatInterval, 5_000);
    assert.equal(config.redis.keyTtl, 60);
});

test('accepts a worker key TTL three times the heartbeat interval', () => {
    const config = parseConfig({
        ...requiredEnvironment,
        HEARTBEAT_INTERVAL: '20',
        REDIS_KEY_TTL: '60',
    });

    assert.equal(config.server.heartbeatInterval, 20_000);
    assert.equal(config.redis.keyTtl, 60);
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
