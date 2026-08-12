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

test('rejects a heartbeat interval that can let the worker key expire', () => {
    assert.throws(
        () => parseConfig({
            ...requiredEnvironment,
            HEARTBEAT_INTERVAL: '60',
            REDIS_KEY_TTL: '60',
        }),
        (error: unknown) => {
            assert.ok(error instanceof ZodError);
            assert.equal(
                error.issues[0]?.message,
                'HEARTBEAT_INTERVAL must be less than REDIS_KEY_TTL'
            );
            return true;
        }
    );
});
