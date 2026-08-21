# Control plane server

The server uses these environment variables:

- `DATABASE_URL` is required. It is the PostgreSQL connection string.
- `LISTEN_ADDR` is optional. Its default value is `:8080`.

The `serve` command applies database migrations before it starts the HTTP
server. API key commands open the database but do not apply migrations.

Manage API keys with the `apikey` command:

```text
server apikey create --name deployment
server apikey list
server apikey revoke --id <uuid>
```

## Authentication bootstrap mode

**A server with zero active API keys serves the entire control plane without
authentication.** Create the first API key to lock the running server and
require a bearer token for all control-plane routes. The server stays locked
for its process lifetime, even if you later revoke every key. Restart the
server to enter bootstrap mode again when the database has zero active keys.
