# API Reference

Technical reference for playwright-distributed HTTP and WebSocket endpoints.

---

## Proxy Endpoints

### WebSocket Connection

**Endpoint**: `ws://host:8080/` or `ws://host:8080`

**Method**: WebSocket Upgrade

**Description**: Connect to a browser in the grid.

#### Query Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `browser` | string | `chromium` | Browser type: `chromium`, `firefox`, or `webkit` |

#### Examples

```
ws://localhost:8080
ws://localhost:8080?browser=chromium
ws://localhost:8080?browser=firefox
ws://localhost:8080?browser=webkit
```

#### Request Headers

Standard WebSocket upgrade headers:

```http
GET /?browser=chromium HTTP/1.1
Host: localhost:8080
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: <base64-encoded-key>
Sec-WebSocket-Version: 13
```

#### Success Response

```http
HTTP/1.1 101 Switching Protocols
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Accept: <calculated-accept>
```

After upgrade, the WebSocket carries Playwright protocol messages.

#### Error Responses

**No available workers**:
```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/json

{"error": "no available servers"}
```

**Invalid browser type**:
```http
HTTP/1.1 400 Bad Request
Content-Type: application/json

{"error": "invalid browser type"}
```

**Connection timeout**:
```http
HTTP/1.1 504 Gateway Timeout
Content-Type: application/json

{"error": "worker selection timeout"}
```

---

### Metrics Endpoint

**Endpoint**: `GET /metrics`

**Description**: Returns current active connection count.

#### Response

```json
{
  "activeConnections": 12
}
```

#### Example

```bash
curl http://localhost:8080/metrics
```

---

### Favicon Endpoint

**Endpoint**: `GET /favicon.ico`

**Description**: Returns 204 No Content (prevents browser favicon errors).

#### Response

```http
HTTP/1.1 204 No Content
```

---

## WebSocket Protocol

After the WebSocket connection is established, all messages use the Playwright CDP (Chrome DevTools Protocol) based format.

### Message Format

Messages are JSON objects:

```json
{
  "id": 1,
  "method": "Browser.newContext",
  "params": {}
}
```

### Response Format

```json
{
  "id": 1,
  "result": {
    "context": {
      "guid": "context@abc123"
    }
  }
}
```

### Event Format

```json
{
  "method": "Page.frameNavigated",
  "params": {
    "frame": { ... }
  }
}
```

The proxy relays these messages unchanged between client and worker.

---

## HTTP Status Codes

| Code | Meaning |
|------|---------|
| 101 | Switching Protocols (WebSocket upgrade successful) |
| 200 | OK |
| 204 | No Content |
| 400 | Bad Request (invalid parameters) |
| 503 | Service Unavailable (no workers available) |
| 504 | Gateway Timeout (worker selection timeout) |

---

## Connection Lifecycle

### 1. Client Connects

```
Client                         Proxy
   │                             │
   │──GET / HTTP/1.1────────────►│
   │  Upgrade: websocket         │
   │  ?browser=chromium          │
```

### 2. Worker Selection

```
Proxy                          Redis
   │                             │
   │──EVALSHA selector.lua ─────►│
   │◄──worker endpoint ──────────│
```

### 3. Backend Connection

```
Proxy                          Worker
   │                             │
   │──WebSocket Connect ────────►│
   │◄──Accept ───────────────────│
```

### 4. Client Upgrade

```
Client                         Proxy
   │                             │
   │◄──101 Switching Protocols───│
```

### 5. Message Relay

```
Client                Proxy                Worker
   │                    │                    │
   │══Playwright ══════►│══════════════════►│
   │◄═══════════════════│◄══════════════════│
```

### 6. Disconnect

```
Client                Proxy                Worker
   │                    │                    │
   │──Close Frame ─────►│──Close Frame ─────►│
   │◄──Close Ack ───────│◄──Close Ack ───────│
```

---

## Timeouts

| Timeout | Default | Description |
|---------|---------|-------------|
| Worker selection | 5s | Max time to find available worker |
| HTTP write | 15s | Max time to send HTTP response |
| WebSocket ping | 60s | Keep-alive interval |

---

## Rate Limits

Currently, playwright-distributed does not implement rate limiting. All requests are processed based on worker availability.

To implement rate limiting, add a reverse proxy (nginx, Traefik) in front of the playwright-distributed proxy.

---

## See Also

- **[Error Codes](error-codes.md)** — Detailed error information
- **[Configuration](../configuration/proxy.md)** — Proxy settings
