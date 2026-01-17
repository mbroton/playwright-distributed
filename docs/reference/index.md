# Reference

Technical reference documentation for playwright-distributed internals.

---

## Reference Guides

### [API Reference](api.md)

HTTP and WebSocket endpoints:
- Proxy endpoints
- Query parameters
- Response formats

### [Redis Schema](redis-schema.md)

Internal data structures:
- Worker registry keys
- Connection counters
- Command keys
- Lua scripts

### [Error Codes](error-codes.md)

Error messages and their meanings:
- HTTP status codes
- WebSocket errors
- Common error messages

---

## Quick Reference

### Proxy Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/` | WebSocket | Browser connection |
| `/metrics` | GET | Active connection count |

### Query Parameters

| Parameter | Values | Default | Purpose |
|-----------|--------|---------|---------|
| `browser` | `chromium`, `firefox`, `webkit` | `chromium` | Select browser type |

### Redis Keys

| Pattern | Type | Purpose |
|---------|------|---------|
| `worker:{browser}:{id}` | Hash | Worker metadata |
| `cluster:active_connections` | Hash | Per-worker active count |
| `cluster:lifetime_connections` | Hash | Per-worker lifetime count |
| `worker:cmd:{browser}:{id}` | String | Shutdown commands |

---

## See Also

- **[Configuration](../configuration/index.md)** — Environment variables
- **[Architecture](../concepts/architecture.md)** — System design
