# Error Codes

Reference for error messages and status codes in playwright-distributed.

---

## HTTP Status Codes

### Success Codes

| Code | Message | Description |
|------|---------|-------------|
| 101 | Switching Protocols | WebSocket upgrade successful |
| 200 | OK | Request successful |
| 204 | No Content | Request successful, no body |

### Client Errors

| Code | Message | Description |
|------|---------|-------------|
| 400 | Bad Request | Invalid request (e.g., bad browser type) |

### Server Errors

| Code | Message | Description |
|------|---------|-------------|
| 503 | Service Unavailable | No workers available |
| 504 | Gateway Timeout | Worker selection timed out |

---

## Proxy Error Messages

### "no available servers"

**HTTP Status**: 503

**Cause**: No workers are available to handle the request.

**Possible reasons**:
- No workers running
- All workers at `MAX_CONCURRENT_SESSIONS`
- All workers at `MAX_LIFETIME_SESSIONS` and draining
- Workers not registered (network/Redis issue)
- Requested browser type has no workers

**Debug steps**:
```bash
# Check if workers are running
docker compose ps

# Check if workers are registered
docker compose exec redis redis-cli KEYS "worker:*"

# Check active connections
docker compose exec redis redis-cli HGETALL cluster:active_connections

# Check worker status
docker compose exec redis redis-cli HGETALL worker:chromium:abc123
```

**Fix**:
- Start workers if not running
- Scale up if at capacity
- Check network connectivity
- Verify browser type matches available workers

---

### "invalid browser type"

**HTTP Status**: 400

**Cause**: The `browser` query parameter has an invalid value.

**Valid values**: `chromium`, `firefox`, `webkit`

**Fix**: Use a valid browser type in your connection URL.

---

### "worker selection timeout"

**HTTP Status**: 504

**Cause**: No worker became available within `WORKER_SELECT_TIMEOUT`.

**Possible reasons**:
- All workers busy and none freed within timeout
- Workers crashing and restarting
- Timeout too short for current load

**Fix**:
- Scale up workers
- Increase `WORKER_SELECT_TIMEOUT`
- Check for worker crashes

---

### "failed to connect to worker"

**HTTP Status**: 503

**Cause**: Proxy selected a worker but couldn't connect to it.

**Possible reasons**:
- Worker crashed between selection and connection
- Network issue between proxy and worker
- `PRIVATE_HOSTNAME` not resolvable from proxy

**Debug steps**:
```bash
# Get worker endpoint
docker compose exec redis redis-cli HGET worker:chromium:abc123 wsEndpoint

# Test connectivity from proxy
docker compose exec proxy wget -O- --timeout=2 <endpoint>
```

**Fix**:
- Check `PRIVATE_HOSTNAME` configuration
- Verify network connectivity
- Check worker logs for crashes

---

## WebSocket Error Messages

### "disconnected"

**Cause**: The WebSocket connection was closed unexpectedly.

**Possible reasons**:
- Worker restarted (lifetime limit reached)
- Worker crashed
- Network interruption
- Proxy restarted

**Client handling**:
```python
try:
    browser = await p.chromium.connect(url)
except PlaywrightError as e:
    if "disconnected" in str(e):
        # Reconnect and retry
        pass
```

---

### "browser has been closed"

**Cause**: Attempted to use a browser that was already closed.

**Possible reasons**:
- Called methods after `browser.close()`
- Worker restarted while browser was in use
- Connection dropped

**Fix**: Ensure you're not using a closed browser. Implement reconnection logic.

---

### "Target closed"

**Cause**: The page or context was closed while an operation was pending.

**Possible reasons**:
- Context was closed during navigation
- Worker restarted
- Timeout exceeded

**Fix**: Handle this error and retry the operation with a new context.

---

## Worker Error Messages

### "REDIS_URL is required"

**Cause**: Worker started without `REDIS_URL` environment variable.

**Fix**: Set `REDIS_URL` in your Docker Compose or deployment config.

---

### "Invalid Redis URL"

**Cause**: The `REDIS_URL` format is incorrect.

**Valid format**: `redis://host:port` or `redis://user:password@host:port`

**Fix**: Check and correct the `REDIS_URL` format.

---

### "PORT is required"

**Cause**: Worker started without `PORT` environment variable.

**Fix**: Set `PORT` in your Docker Compose or deployment config.

---

### "Failed to connect to Redis"

**Cause**: Worker can't connect to Redis.

**Possible reasons**:
- Redis not running
- Wrong `REDIS_URL`
- Network issue
- Redis not accepting connections

**Debug steps**:
```bash
# Check Redis is running
docker compose ps redis

# Check connectivity
docker compose exec worker nc -zv redis 6379
```

**Fix**:
- Start Redis
- Correct `REDIS_URL`
- Check network/firewall

---

### "Browser failed to launch"

**Cause**: Playwright couldn't start the browser process.

**Possible reasons**:
- Missing browser binaries
- Insufficient memory
- Missing system dependencies

**Fix**:
- Use the official Playwright Docker image
- Increase memory limits
- Check worker logs for specific error

---

## Troubleshooting by Error

| Error | First Check | Likely Fix |
|-------|-------------|------------|
| "no available servers" | Worker count | Scale up or fix registration |
| "disconnected" | Worker logs | Fix crashes or network |
| "connection timeout" | Network | Check PRIVATE_HOSTNAME |
| "Redis connection failed" | Redis running | Start Redis, check URL |

---

## Logging Errors

Enable debug logging to see detailed error information:

```yaml
environment:
  - LOG_LEVEL=debug
```

Then check logs:
```bash
docker compose logs -f proxy
docker compose logs -f worker
```

---

## See Also

- **[Troubleshooting](../operations/troubleshooting.md)** — Step-by-step debug guide
- **[Configuration](../configuration/index.md)** — Check settings
- **[Networking](../configuration/networking.md)** — Network issues
